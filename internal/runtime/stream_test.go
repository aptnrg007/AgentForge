package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"agentforge/internal/message"
	"agentforge/internal/provider"
)

// deltaStream yields a fixed sequence of text deltas before completing
// with a pre-built response, so a test can observe events arriving one at
// a time instead of only ever seeing the fully assembled result (which is
// all fakeProvider's default Stream — via provider.NewResponseStream —
// would show).
type deltaStream struct {
	deltas []string
	idx    int
	resp   *provider.Response
}

func (s *deltaStream) Next() bool {
	if s.idx >= len(s.deltas) {
		return false
	}
	s.idx++
	return true
}
func (s *deltaStream) Delta() string                         { return s.deltas[s.idx-1] }
func (s *deltaStream) Response() (*provider.Response, error) { return s.resp, nil }
func (s *deltaStream) Err() error                            { return nil }
func (s *deltaStream) Close() error                          { return nil }

// countingProvider scripts Complete's responses exactly like fakeProvider,
// but also counts how many times Complete vs. Stream was actually called
// — used to prove the engine picks the right one depending on whether an
// event sink is installed.
type countingProvider struct {
	responses     []*provider.Response
	calls         int
	completeCalls int
	streamCalls   int
}

func (p *countingProvider) Name() string { return "counting-fake" }

func (p *countingProvider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	p.completeCalls++
	if p.calls >= len(p.responses) {
		return nil, fmt.Errorf("countingProvider: no more scripted responses")
	}
	resp := p.responses[p.calls]
	p.calls++
	return resp, nil
}

func (p *countingProvider) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	p.streamCalls++
	if p.calls >= len(p.responses) {
		return nil, fmt.Errorf("countingProvider: no more scripted responses")
	}
	resp := p.responses[p.calls]
	p.calls++
	return provider.NewResponseStream(resp), nil
}

func (p *countingProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

// deltaProvider always streams a scripted sequence of deltas via Stream and
// fails the test if Complete is ever called — used to prove the engine
// takes the streaming path (and forwards deltas) when a sink is installed.
type deltaProvider struct {
	t      *testing.T
	deltas []string
	resp   *provider.Response
}

func (p *deltaProvider) Name() string { return "delta-fake" }

func (p *deltaProvider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	p.t.Fatal("Complete must not be called when an event sink is installed")
	return nil, nil
}

func (p *deltaProvider) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	return &deltaStream{deltas: p.deltas, resp: p.resp}, nil
}

func (p *deltaProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

func TestTokenEventsArriveInOrderAndConcatenate(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &deltaProvider{t: t, deltas: []string{"Hel", "lo, ", "world"}, resp: textResponse("Hello, world")}
	eng := NewEngine(st, fp, Config{AgentName: "test-agent", Model: "test-model", MaxTurns: 10})

	var tokens []string
	eng.OnEvent(func(ev Event) {
		if ev.Kind == EventToken {
			tokens = append(tokens, ev.Text)
		}
	})

	runID := "run-tokens"
	if err := eng.NewRun(ctx, runID, "hi"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state := runToTerminal(t, eng, runID); state != StateCompleted {
		t.Fatalf("expected completed, got %s", state)
	}

	if got := fmt.Sprint(tokens); got != `[Hel lo,  world]` {
		t.Fatalf("expected deltas in order [Hel, lo, , world], got %v", tokens)
	}

	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var assembled string
	for _, b := range tokens {
		assembled += b
	}
	if assembled != "Hello, world" {
		t.Fatalf("deltas should concatenate to the final text, got %q", assembled)
	}
	if len(msgs) < 2 || msgs[1].Content[0].Text != "Hello, world" {
		t.Fatalf("expected the persisted assistant message to be the fully assembled text, got %+v", msgs)
	}
}

func TestToolCallAndToolResultEventsFireWithCorrectIDs(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	mixedToolUse := &provider.Response{
		Content: []message.ContentBlock{
			{Type: message.BlockToolUse, ID: "call_echo", Name: "echo", Input: json.RawMessage(`{"message":"hi"}`)},
			{Type: message.BlockToolUse, ID: "call_danger", Name: "danger", Input: json.RawMessage(`{}`)},
		},
		StopReason: "tool_use",
	}
	fp := &fakeProvider{responses: []*provider.Response{mixedToolUse, textResponse("done")}}

	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Approvals: ApprovalPolicy{Require: []string{"danger"}},
	})
	eng.RegisterTool(NewEchoTool())
	eng.RegisterTool(Tool{
		Name:        "danger",
		InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			t.Fatal("a denied tool must never execute")
			return "", nil
		},
	})

	var calls []Event
	var results []Event
	eng.OnEvent(func(ev Event) {
		switch ev.Kind {
		case EventToolCall:
			calls = append(calls, ev)
		case EventToolResult:
			results = append(results, ev)
		}
	})

	runID := "run-tool-events"
	if err := eng.NewRun(ctx, runID, "do things"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	if state, err := eng.Step(ctx, runID); err != nil || state != StateAwaitingApproval {
		t.Fatalf("expected awaiting_approval, got %s (err=%v)", state, err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected two tool_call events emitted before any decision, got %d: %+v", len(calls), calls)
	}
	if calls[0].CallID != "call_echo" || calls[0].ToolName != "echo" {
		t.Fatalf("expected first tool_call for call_echo/echo, got %+v", calls[0])
	}
	if calls[1].CallID != "call_danger" || calls[1].ToolName != "danger" {
		t.Fatalf("expected second tool_call for call_danger/danger, got %+v", calls[1])
	}
	if len(results) != 0 {
		t.Fatalf("no tool_result events should fire before tools run, got %+v", results)
	}

	if _, err := eng.RecordApproval(ctx, runID, "call_danger", "denied", "user", "no"); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}

	if state, err := eng.Step(ctx, runID); err != nil || state != StateReadyForModel {
		t.Fatalf("expected ready_for_model after tools ran, got %s (err=%v)", state, err)
	}

	if len(results) != 2 {
		t.Fatalf("expected two tool_result events, got %d: %+v", len(results), results)
	}
	byID := map[string]Event{results[0].CallID: results[0], results[1].CallID: results[1]}
	echoResult, ok := byID["call_echo"]
	if !ok || echoResult.IsError {
		t.Fatalf("expected a successful echo tool_result, got %+v", byID)
	}
	if echoResult.Result != "hi" {
		t.Fatalf("expected the echo tool to return its input, got %q", echoResult.Result)
	}
	dangerResult, ok := byID["call_danger"]
	if !ok || !dangerResult.IsError {
		t.Fatalf("expected an error tool_result for the denied call, got %+v", byID)
	}
}

// TestNoEventSinkStillUsesComplete is a regression guard for the whole
// feature being additive: an Engine with no OnEvent callback installed
// must behave exactly as it did before streaming existed, calling
// Complete and never Stream.
func TestNoEventSinkStillUsesComplete(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &countingProvider{responses: []*provider.Response{textResponse("hi there")}}
	eng := NewEngine(st, fp, Config{AgentName: "test-agent", Model: "test-model", MaxTurns: 10})
	// Deliberately no eng.OnEvent call.

	runID := "run-no-sink"
	if err := eng.NewRun(ctx, runID, "hello"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state := runToTerminal(t, eng, runID); state != StateCompleted {
		t.Fatalf("expected completed, got %s", state)
	}

	if fp.completeCalls != 1 {
		t.Fatalf("expected exactly one Complete call, got %d", fp.completeCalls)
	}
	if fp.streamCalls != 0 {
		t.Fatalf("expected Stream to never be called with no event sink installed, got %d calls", fp.streamCalls)
	}
}
