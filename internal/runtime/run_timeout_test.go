package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentforge/internal/provider"
)

// TestStepEnforcesRunTimeoutDuringProviderCall proves Config.RunTimeout
// (wired from limits.timeout by agent.Build) actually bounds a run's
// total wall-clock duration: with no external cancellation at all — a
// plain context.Background() — the deadline Step derives from the run's
// own CreatedAt is what stops a model call that would otherwise hang
// forever. Ends at StateFailed, not StateCancelled: a policy limit being
// hit isn't the same thing as an external stop request.
func TestStepEnforcesRunTimeoutDuringProviderCall(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	eng := NewEngine(st, waitForDoneThenComplete{}, Config{AgentName: "a", Model: "m", MaxTurns: 10, RunTimeout: 20 * time.Millisecond})

	ctx := context.Background()
	if err := eng.NewRun(ctx, "r", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	state, err := eng.Step(ctx, "r") // blocks until RunTimeout's derived deadline fires
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != StateFailed {
		t.Fatalf("state = %s, want failed", state)
	}

	run, err := st.GetRun(ctx, "r")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "exceeded its time limit") {
		t.Fatalf("run.Error = %v, want it to mention the time limit", run.Error)
	}
}

// TestStepRunTimeoutAlreadyExpiredFailsWithoutHanging covers the resume
// case: a run whose RunTimeout deadline has already passed by the time
// something next calls Step (e.g. the process was down for a while).
// Uses a plain fakeProvider that would otherwise succeed immediately, to
// prove the deadline is caught before — or regardless of — the provider
// call, not just when the provider itself happens to block.
func TestStepRunTimeoutAlreadyExpiredFailsWithoutHanging(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	fp := &fakeProvider{responses: []*provider.Response{textResponse("should not matter either way")}}
	eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10, RunTimeout: time.Millisecond})

	ctx := context.Background()
	if err := eng.NewRun(ctx, "r", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // let the 1ms RunTimeout actually elapse

	state, err := eng.Step(ctx, "r")
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != StateFailed {
		t.Fatalf("state = %s, want failed", state)
	}
	run, err := st.GetRun(ctx, "r")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "exceeded its time limit") {
		t.Fatalf("run.Error = %v, want it to mention the time limit", run.Error)
	}
}

// TestStepToolExecutionEndsRunOnRunTimeout proves the run-level deadline
// reaches stepTools too, not just stepModel — RunTimeout is meant to
// bound the whole run end to end — and that it's reported correctly:
// "run exceeded its time limit", not a tool_policy-flavored "tool %q
// timed out after 0s" (tool_policy isn't configured in this test at all,
// so timeoutFor would return 0, which is exactly the misleading message
// this is guarding against).
func TestStepToolExecutionEndsRunOnRunTimeout(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	fp := &fakeProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "hang.tool", `{}`),
	}}
	eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10, RunTimeout: 20 * time.Millisecond})
	eng.RegisterTool(newBlockingTool("hang.tool"))

	ctx := context.Background()
	if err := eng.NewRun(ctx, "r", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	state, err := eng.Step(ctx, "r") // model call: tool_use -> ready_for_tools
	if err != nil {
		t.Fatalf("Step (model): %v", err)
	}
	if state != StateReadyForTools {
		t.Fatalf("state after model turn = %s, want ready_for_tools", state)
	}

	state, err = eng.Step(ctx, "r") // tool execution: blocks until RunTimeout fires
	if err != nil {
		t.Fatalf("Step (tools): %v", err)
	}
	if state != StateFailed {
		t.Fatalf("state = %s, want failed", state)
	}

	run, err := st.GetRun(ctx, "r")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "exceeded its time limit") {
		t.Fatalf("run.Error = %v, want it to mention the time limit, not a tool_policy timeout", run.Error)
	}

	calls, err := st.ListToolCalls(ctx, "r")
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(calls) != 1 || calls[0].Result == nil {
		t.Fatalf("expected the blocking call's own result to still be recorded, got %+v", calls)
	}
}

// TestStepRunTimeoutZeroIsUnbounded is a direct sanity check that
// RunTimeout's zero value (every Config{} literal elsewhere in this test
// suite already relies on this implicitly) really does mean "no
// deadline" — a slow-but-eventually-responding provider still completes.
func TestStepRunTimeoutZeroIsUnbounded(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	fp := &fakeProvider{responses: []*provider.Response{textResponse("done")}}
	eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10}) // RunTimeout left at zero value

	ctx := context.Background()
	if err := eng.NewRun(ctx, "r", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state := runToTerminal(t, eng, "r"); state != StateCompleted {
		t.Fatalf("state = %s, want completed", state)
	}
}
