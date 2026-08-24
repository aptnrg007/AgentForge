package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"agentforge/internal/message"
	"agentforge/internal/provider"
	"agentforge/internal/store"
)

// fakeProvider replays a scripted sequence of responses, one per call. This
// is the fixture-replay approach from docs/DESIGN.md section 11:
// deterministic, fast, and independent of a live Ollama instance.
type fakeProvider struct {
	responses []*provider.Response
	calls     int
	// requests records every Request Complete was called with, so tests
	// can assert on what the engine actually sent (e.g. ResponseSchema).
	requests []provider.Request
	// caps lets a test opt into providers.Capabilities.StructuredOutput
	// etc.; the zero value matches every pre-existing test's expectation
	// of an all-false Capabilities().
	caps provider.Capabilities
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	f.requests = append(f.requests, r)
	if f.calls >= len(f.responses) {
		return nil, fmt.Errorf("fake provider: no more scripted responses (call %d)", f.calls)
	}
	resp := f.responses[f.calls]
	f.calls++
	return resp, nil
}

func (f *fakeProvider) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	resp, err := f.Complete(ctx, r)
	if err != nil {
		return nil, err
	}
	return provider.NewResponseStream(resp), nil
}

func (f *fakeProvider) Capabilities() provider.Capabilities { return f.caps }

func textResponse(text string) *provider.Response {
	return &provider.Response{
		Content:    []message.ContentBlock{{Type: message.BlockText, Text: text}},
		StopReason: "end_turn",
	}
}

func toolUseResponse(id, name, input string) *provider.Response {
	return &provider.Response{
		Content:    []message.ContentBlock{{Type: message.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}},
		StopReason: "tool_use",
	}
}

func openTestStore(t *testing.T, dbPath string) *store.Store {
	t.Helper()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func runToTerminal(t *testing.T, eng *Engine, runID string) State {
	t.Helper()
	ctx := context.Background()
	var state State
	for i := 0; i < 10; i++ {
		s, err := eng.Step(ctx, runID)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		state = s
		if state == StateCompleted || state == StateFailed {
			return state
		}
	}
	t.Fatalf("run did not reach a terminal state within 10 steps (stuck at %s)", state)
	return state
}

func TestHappyPathEndToEnd(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "echo", `{"message":"hello"}`),
		textResponse("done: hello"),
	}}

	eng := NewEngine(st, fp, Config{AgentName: "test-agent", Model: "test-model", MaxTurns: 10})
	eng.RegisterTool(NewEchoTool())

	runID := "run-happy"
	if err := eng.NewRun(ctx, runID, "please echo hello"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if state := runToTerminal(t, eng, runID); state != StateCompleted {
		run, _ := st.GetRun(ctx, runID)
		t.Fatalf("expected completed, got %s (error=%v)", state, run.Error)
	}

	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	// user, assistant(tool_use), tool(tool_result), assistant(text)
	if len(msgs) != 4 {
		t.Fatalf("expected 4 persisted messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[2].Role != message.RoleTool || msgs[2].Content[0].Content != "hello" {
		t.Fatalf("expected tool result 'hello', got %+v", msgs[2])
	}

	calls, err := st.ListToolCalls(ctx, runID)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].ToolName != "echo" {
		t.Fatalf("expected one echo tool call, got %+v", calls)
	}
}

func TestResumeAfterRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "resume.db")

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "echo", `{"message":"hi"}`),
		textResponse("all done"),
	}}
	cfg := Config{AgentName: "test-agent", Model: "test-model", MaxTurns: 10}
	runID := "run-resume"

	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	eng1 := NewEngine(st1, fp, cfg)
	eng1.RegisterTool(NewEchoTool())

	if err := eng1.NewRun(ctx, runID, "echo hi"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state, err := eng1.Step(ctx, runID); err != nil {
		t.Fatalf("Step 1: %v", err)
	} else if state != StateReadyForTools {
		t.Fatalf("expected ready_for_tools before restart, got %s", state)
	}

	// Simulate a process restart: close this store handle entirely, then
	// open a brand new Store + Engine against the same file and keep going.
	if err := st1.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	st2 := openTestStore(t, dbPath)
	eng2 := NewEngine(st2, fp, cfg)
	eng2.RegisterTool(NewEchoTool())

	if state := runToTerminal(t, eng2, runID); state != StateCompleted {
		run, _ := st2.GetRun(ctx, runID)
		t.Fatalf("expected completed after resume, got %s (error=%v)", state, run.Error)
	}
}

func TestToolCallRepairThenFail(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	// Three consecutive malformed calls: the first two are repaired
	// (return to ready_for_model), the third exceeds maxRepairAttempts.
	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "bogus.tool", `{}`),
		toolUseResponse("call_2", "bogus.tool", `{}`),
		toolUseResponse("call_3", "bogus.tool", `{}`),
	}}

	eng := NewEngine(st, fp, Config{AgentName: "test-agent", Model: "test-model", MaxTurns: 10})
	eng.RegisterTool(NewEchoTool())

	runID := "run-repair"
	if err := eng.NewRun(ctx, runID, "call a tool that doesn't exist"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if state := runToTerminal(t, eng, runID); state != StateFailed {
		t.Fatalf("expected failed after repeated malformed tool calls, got %s", state)
	}
	if fp.calls != 3 {
		t.Fatalf("expected exactly 3 provider calls (initial + 2 repairs), got %d", fp.calls)
	}

	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Error == nil || *run.Error == "" {
		t.Fatalf("expected run.Error to be set")
	}
}

func TestMaxTurnsEnforced(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "echo", `{"message":"a"}`),
		toolUseResponse("call_2", "echo", `{"message":"b"}`),
	}}

	eng := NewEngine(st, fp, Config{AgentName: "test-agent", Model: "test-model", MaxTurns: 2})
	eng.RegisterTool(NewEchoTool())

	runID := "run-maxturns"
	if err := eng.NewRun(ctx, runID, "keep calling echo forever"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if state := runToTerminal(t, eng, runID); state != StateFailed {
		t.Fatalf("expected failed due to max_turns, got %s", state)
	}
	if fp.calls != 2 {
		t.Fatalf("expected exactly 2 provider calls before max_turns cut it off, got %d", fp.calls)
	}

	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Error == nil {
		t.Fatalf("expected run.Error to describe the max_turns failure")
	}
}

// newCountingTool returns a stub tool alongside a pointer to a counter
// incremented on every Execute call, so tests can assert a denied or
// not-yet-decided call was never actually run.
func newCountingTool(name string, readOnly bool) (Tool, *int) {
	calls := 0
	tool := Tool{
		Name:         name,
		Description:  "test tool",
		InputSchema:  json.RawMessage(`{}`),
		ReadOnlyHint: readOnly,
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			calls++
			return "ok", nil
		},
	}
	return tool, &calls
}

func TestRequireApprovalPausesRunWithPendingCall(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{"x":1}`),
		textResponse("done"),
	}}
	tool, calls := newCountingTool("danger.tool", false)

	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Approvals: ApprovalPolicy{Require: []string{"danger.tool"}},
	})
	eng.RegisterTool(tool)

	runID := "run-require"
	if err := eng.NewRun(ctx, runID, "do the dangerous thing"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	state, err := eng.Step(ctx, runID)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != StateAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %s", state)
	}
	if *calls != 0 {
		t.Fatalf("expected the tool not to run before approval, got %d calls", *calls)
	}

	pending, err := st.ListPendingApprovals(ctx, runID)
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	if len(pending) != 1 || pending[0].ToolName != "danger.tool" {
		t.Fatalf("expected one pending call for danger.tool, got %+v", pending)
	}

	state, err = eng.RecordApproval(ctx, runID, pending[0].ID, "approved", "tester", "")
	if err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	if state != StateReadyForTools {
		t.Fatalf("expected ready_for_tools after approval, got %s", state)
	}

	if final := runToTerminal(t, eng, runID); final != StateCompleted {
		t.Fatalf("expected completed, got %s", final)
	}
	if *calls != 1 {
		t.Fatalf("expected the tool to run exactly once after approval, got %d", *calls)
	}
}

func TestDenialAppendsErrorResultAndAgentTriesAlternative(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{"x":1}`),
		textResponse("okay, I will try a different approach instead"),
	}}
	tool, calls := newCountingTool("danger.tool", false)

	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Approvals: ApprovalPolicy{Require: []string{"danger.tool"}},
	})
	eng.RegisterTool(tool)

	runID := "run-deny"
	if err := eng.NewRun(ctx, runID, "do the dangerous thing"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if state, err := eng.Step(ctx, runID); err != nil {
		t.Fatalf("Step: %v", err)
	} else if state != StateAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %s", state)
	}

	pending, err := st.ListPendingApprovals(ctx, runID)
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected one pending call, got %d", len(pending))
	}

	state, err := eng.RecordApproval(ctx, runID, pending[0].ID, "denied", "tester", "too risky")
	if err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	if state != StateReadyForTools {
		t.Fatalf("expected ready_for_tools after denial, got %s", state)
	}

	// The run must not end here: denial feeds an error result back to the
	// model, which gets another turn to try something else.
	if final := runToTerminal(t, eng, runID); final != StateCompleted {
		t.Fatalf("expected the run to complete after trying an alternative, got %s", final)
	}
	if *calls != 0 {
		t.Fatalf("expected the denied tool to never execute, got %d calls", *calls)
	}

	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var found bool
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == message.BlockToolResult && b.IsError && b.Content == "tool call denied: too risky" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected a tool_result explaining the denial, got messages: %+v", msgs)
	}
}

func TestApprovalTimeoutDeny(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{}`),
		textResponse("moved on without it"),
	}}
	tool, calls := newCountingTool("danger.tool", false)

	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Approvals: ApprovalPolicy{
			Require:   []string{"danger.tool"},
			Timeout:   20 * time.Millisecond,
			OnTimeout: "deny",
		},
	})
	eng.RegisterTool(tool)

	runID := "run-timeout-deny"
	if err := eng.NewRun(ctx, runID, "do it"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state, err := eng.Step(ctx, runID); err != nil {
		t.Fatalf("Step: %v", err)
	} else if state != StateAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %s", state)
	}

	time.Sleep(40 * time.Millisecond)

	// No human decision was ever recorded; the next Step must notice the
	// timeout itself and apply on_timeout: deny.
	if final := runToTerminal(t, eng, runID); final != StateCompleted {
		t.Fatalf("expected completed after the timeout resolved the call, got %s", final)
	}
	if *calls != 0 {
		t.Fatalf("expected on_timeout: deny to skip execution, got %d calls", *calls)
	}

	toolCalls, err := st.ListToolCalls(ctx, runID)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(toolCalls) != 1 || toolCalls[0].Approval != "denied" {
		t.Fatalf("expected the call to be auto-denied by timeout, got %+v", toolCalls)
	}
	if toolCalls[0].DecidedBy == nil || *toolCalls[0].DecidedBy != "system:timeout" {
		t.Fatalf("expected decided_by=system:timeout, got %v", toolCalls[0].DecidedBy)
	}
}

func TestApprovalTimeoutAllow(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{}`),
		textResponse("done"),
	}}
	tool, calls := newCountingTool("danger.tool", false)

	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Approvals: ApprovalPolicy{
			Require:   []string{"danger.tool"},
			Timeout:   20 * time.Millisecond,
			OnTimeout: "allow",
		},
	})
	eng.RegisterTool(tool)

	runID := "run-timeout-allow"
	if err := eng.NewRun(ctx, runID, "do it"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state, err := eng.Step(ctx, runID); err != nil {
		t.Fatalf("Step: %v", err)
	} else if state != StateAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %s", state)
	}

	time.Sleep(40 * time.Millisecond)

	if final := runToTerminal(t, eng, runID); final != StateCompleted {
		t.Fatalf("expected completed after the timeout resolved the call, got %s", final)
	}
	if *calls != 1 {
		t.Fatalf("expected on_timeout: allow to execute the tool, got %d calls", *calls)
	}
}

func TestAnnotatedModeAutoApprovesReadOnlyAndPausesOnMutating(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	safeTool, safeCalls := newCountingTool("safe.tool", true)
	riskyTool, riskyCalls := newCountingTool("risky.tool", false)

	newEngine := func(fp *fakeProvider) *Engine {
		eng := NewEngine(st, fp, Config{
			AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
			Approvals: ApprovalPolicy{Mode: "annotated"},
		})
		eng.RegisterTool(safeTool)
		eng.RegisterTool(riskyTool)
		return eng
	}

	t.Run("read-only tool auto-approved", func(t *testing.T) {
		fp := &fakeProvider{responses: []*provider.Response{
			toolUseResponse("call_1", "safe.tool", `{}`),
			textResponse("done"),
		}}
		eng := newEngine(fp)
		runID := "run-annotated-safe"
		if err := eng.NewRun(ctx, runID, "use the safe tool"); err != nil {
			t.Fatalf("NewRun: %v", err)
		}
		state, err := eng.Step(ctx, runID)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if state != StateReadyForTools {
			t.Fatalf("expected ready_for_tools (auto-approved), got %s", state)
		}
		if final := runToTerminal(t, eng, runID); final != StateCompleted {
			t.Fatalf("expected completed, got %s", final)
		}
		if *safeCalls != 1 {
			t.Fatalf("expected the read-only tool to run, got %d calls", *safeCalls)
		}
	})

	t.Run("mutating tool pauses for approval", func(t *testing.T) {
		fp := &fakeProvider{responses: []*provider.Response{
			toolUseResponse("call_2", "risky.tool", `{}`),
		}}
		eng := newEngine(fp)
		runID := "run-annotated-risky"
		if err := eng.NewRun(ctx, runID, "use the risky tool"); err != nil {
			t.Fatalf("NewRun: %v", err)
		}
		state, err := eng.Step(ctx, runID)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if state != StateAwaitingApproval {
			t.Fatalf("expected awaiting_approval (no read-only hint), got %s", state)
		}
		if *riskyCalls != 0 {
			t.Fatalf("expected the mutating tool not to run without approval, got %d calls", *riskyCalls)
		}
	})
}
