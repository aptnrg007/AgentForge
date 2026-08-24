package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"agentforge/internal/provider"
)

// TestEngineRunStepsToCompletion confirms Run — the shared core the CLI
// and API drive loops both call into (see Run's doc comment) — actually
// loops through ready_for_model/ready_for_tools instead of stopping
// after one Step, by scripting a tool call followed by a final answer.
func TestEngineRunStepsToCompletion(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "echo", `{"message":"hi"}`),
		textResponse("done"),
	}}
	eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10})
	eng.RegisterTool(Tool{
		Name: "echo",
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "echoed", nil
		},
	})

	ctx := context.Background()
	if err := eng.NewRun(ctx, "r", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	state, err := eng.Run(ctx, "r")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state != StateCompleted {
		t.Fatalf("state = %s, want completed", state)
	}
}

// TestEngineRunStopsAtAwaitingApproval confirms Run stops (rather than
// looping forever) the first time a run needs a human decision.
func TestEngineRunStopsAtAwaitingApproval(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{}`),
	}}
	eng := NewEngine(st, fp, Config{
		AgentName: "a", Model: "m", MaxTurns: 10,
		Approvals: ApprovalPolicy{Mode: "always"},
	})
	eng.RegisterTool(Tool{Name: "danger.tool", Execute: func(ctx context.Context, input json.RawMessage) (string, error) { return "", nil }})

	ctx := context.Background()
	if err := eng.NewRun(ctx, "r", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	state, err := eng.Run(ctx, "r")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if state != StateAwaitingApproval {
		t.Fatalf("state = %s, want awaiting_approval", state)
	}
}

// TestEngineRunAlreadyDeadContextEndsRunWithoutCallingStep confirms Run's
// pre-check (ctx already Done() before the first Step) persists a
// terminal state via EndIfContextDone rather than calling Step with a
// context it already knows is dead.
func TestEngineRunAlreadyDeadContextEndsRunWithoutCallingStep(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	fp := &fakeProvider{responses: []*provider.Response{textResponse("should never be reached")}}
	eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10})

	bg := context.Background()
	if err := eng.NewRun(bg, "r", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	deadCtx, cancel := context.WithCancel(bg)
	cancel()

	if _, err := eng.Run(deadCtx, "r"); err == nil {
		t.Fatal("expected Run to return an error for an already-dead ctx")
	}

	run, err := st.GetRun(bg, "r")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != string(StateCancelled) {
		t.Fatalf("persisted state = %s, want cancelled", run.State)
	}
}
