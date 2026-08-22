package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"agentforge/internal/provider"
)

// newApprovalTool is like newCountingTool but also sets RequiresApproval,
// exercising the field internal/tools sets on every command:-backed
// ToolDefinition.
func newApprovalTool(name string, requiresApproval bool) (Tool, *int) {
	calls := 0
	tool := Tool{
		Name:             name,
		Description:      "test tool",
		InputSchema:      json.RawMessage(`{}`),
		RequiresApproval: requiresApproval,
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			calls++
			return "ok", nil
		},
	}
	return tool, &calls
}

// TestRequiresApprovalGatesUnderDefaultMode proves a tool with
// RequiresApproval set pauses the run even though ApprovalPolicy.Mode is
// left at its default ("never" == auto-run) — the case
// exec-backed tool_definitions rely on so a config that forgets
// approvals: doesn't hand the model unguarded process execution.
func TestRequiresApprovalGatesUnderDefaultMode(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	tool, calls := newApprovalTool("exec.tool", true)
	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "exec.tool", `{}`),
	}}

	eng := NewEngine(st, fp, Config{AgentName: "test-agent", Model: "test-model", MaxTurns: 10})
	eng.RegisterTool(tool)

	runID := "run-requires-approval"
	if err := eng.NewRun(ctx, runID, "run the exec tool"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	state, err := eng.Step(ctx, runID)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != StateAwaitingApproval {
		t.Fatalf("expected awaiting_approval under default mode, got %s", state)
	}
	if *calls != 0 {
		t.Fatalf("expected the tool not to run before approval, got %d calls", *calls)
	}
}

// TestRequiresApprovalOrdinaryToolUnaffected proves the new field is
// opt-in: a tool that doesn't set it keeps auto-running under the
// default mode exactly as before.
func TestRequiresApprovalOrdinaryToolUnaffected(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	tool, calls := newApprovalTool("plain.tool", false)
	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "plain.tool", `{}`),
		textResponse("done"),
	}}

	eng := NewEngine(st, fp, Config{AgentName: "test-agent", Model: "test-model", MaxTurns: 10})
	eng.RegisterTool(tool)

	runID := "run-plain-tool"
	if err := eng.NewRun(ctx, runID, "run the plain tool"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if final := runToTerminal(t, eng, runID); final != StateCompleted {
		t.Fatalf("expected completed, got %s", final)
	}
	if *calls != 1 {
		t.Fatalf("expected the plain tool to run once, got %d", *calls)
	}
}

// TestRequiresApprovalAutoApproveOptsOut proves approvals.auto_approve
// overrides RequiresApproval, the documented way to opt a command tool
// out of the default gate.
func TestRequiresApprovalAutoApproveOptsOut(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	tool, calls := newApprovalTool("exec.tool", true)
	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "exec.tool", `{}`),
		textResponse("done"),
	}}

	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Approvals: ApprovalPolicy{AutoApprove: []string{"exec.tool"}},
	})
	eng.RegisterTool(tool)

	runID := "run-auto-approve-opt-out"
	if err := eng.NewRun(ctx, runID, "run the exec tool"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if final := runToTerminal(t, eng, runID); final != StateCompleted {
		t.Fatalf("expected completed, got %s", final)
	}
	if *calls != 1 {
		t.Fatalf("expected the tool to run once (auto_approve overriding RequiresApproval), got %d", *calls)
	}
}

// TestRequiresApprovalRequireStillWins proves approvals.require still
// forces "pending" even for a tool that isn't RequiresApproval — the
// existing precedence, unchanged by this field's addition.
func TestRequiresApprovalRequireStillWins(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	tool, calls := newApprovalTool("plain.tool", false)
	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "plain.tool", `{}`),
	}}

	eng := NewEngine(st, fp, Config{
		AgentName: "test-agent", Model: "test-model", MaxTurns: 10,
		Approvals: ApprovalPolicy{Require: []string{"plain.tool"}},
	})
	eng.RegisterTool(tool)

	runID := "run-require-still-wins"
	if err := eng.NewRun(ctx, runID, "run the plain tool"); err != nil {
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
}
