package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"agentforge/internal/message"
	"agentforge/internal/provider"
	"agentforge/internal/store"
)

// fakeProvider replays a scripted sequence of responses, one per call. This
// is the fixture-replay approach from PLAN.md section 11: deterministic,
// fast, and independent of a live Ollama instance.
type fakeProvider struct {
	responses []*provider.Response
	calls     int
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	if f.calls >= len(f.responses) {
		return nil, fmt.Errorf("fake provider: no more scripted responses (call %d)", f.calls)
	}
	resp := f.responses[f.calls]
	f.calls++
	return resp, nil
}

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
