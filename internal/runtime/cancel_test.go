package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentforge/internal/provider"
)

// TestCancelMovesEveryNonTerminalStateToCancelled proves Engine.Cancel
// works from all three non-terminal states — unlike RecordApproval, it
// doesn't require pending calls to already be decided.
func TestCancelMovesEveryNonTerminalStateToCancelled(t *testing.T) {
	ctx := context.Background()

	t.Run("ready_for_model", func(t *testing.T) {
		st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
		fp := &fakeProvider{} // Cancel happens before any Step, so no scripted response is needed
		eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10})
		if err := eng.NewRun(ctx, "r", "go"); err != nil {
			t.Fatalf("NewRun: %v", err)
		}
		if run, _ := st.GetRun(ctx, "r"); State(run.State) != StateReadyForModel {
			t.Fatalf("precondition: expected ready_for_model, got %s", run.State)
		}
		if state, err := eng.Cancel(ctx, "r"); err != nil || state != StateCancelled {
			t.Fatalf("Cancel: state=%s err=%v, want cancelled/nil", state, err)
		}
		run, err := st.GetRun(ctx, "r")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if State(run.State) != StateCancelled {
			t.Fatalf("persisted state = %s, want cancelled", run.State)
		}
	})

	t.Run("ready_for_tools", func(t *testing.T) {
		st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
		fp := &fakeProvider{responses: []*provider.Response{
			toolUseResponse("call_1", "echo", `{"message":"hi"}`),
		}}
		eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10})
		eng.RegisterTool(NewEchoTool())
		if err := eng.NewRun(ctx, "r", "go"); err != nil {
			t.Fatalf("NewRun: %v", err)
		}
		state, err := eng.Step(ctx, "r") // model call only — auto-approved tool call, not yet executed
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if state != StateReadyForTools {
			t.Fatalf("precondition: expected ready_for_tools, got %s", state)
		}
		if state, err := eng.Cancel(ctx, "r"); err != nil || state != StateCancelled {
			t.Fatalf("Cancel: state=%s err=%v, want cancelled/nil", state, err)
		}
	})

	t.Run("awaiting_approval", func(t *testing.T) {
		st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
		tool, _ := newApprovalTool("danger.tool", true)
		fp := &fakeProvider{responses: []*provider.Response{
			toolUseResponse("call_1", "danger.tool", `{}`),
		}}
		eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10})
		eng.RegisterTool(tool)
		if err := eng.NewRun(ctx, "r", "go"); err != nil {
			t.Fatalf("NewRun: %v", err)
		}
		state, err := eng.Step(ctx, "r")
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if state != StateAwaitingApproval {
			t.Fatalf("precondition: expected awaiting_approval, got %s", state)
		}
		if state, err := eng.Cancel(ctx, "r"); err != nil || state != StateCancelled {
			t.Fatalf("Cancel: state=%s err=%v, want cancelled/nil", state, err)
		}
	})
}

// TestCancelRejectsAlreadyTerminalRun proves a run that finished, failed,
// or was already cancelled can't be cancelled again — there's nothing
// left to stop, and silently succeeding would hide the run's real
// terminal state from whoever called Cancel.
func TestCancelRejectsAlreadyTerminalRun(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	fp := &fakeProvider{responses: []*provider.Response{textResponse("done")}}
	eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10})
	if err := eng.NewRun(ctx, "r", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if state := runToTerminal(t, eng, "r"); state != StateCompleted {
		t.Fatalf("precondition: expected completed, got %s", state)
	}

	if _, err := eng.Cancel(ctx, "r"); err == nil {
		t.Fatal("expected Cancel to reject an already-completed run")
	} else if !errors.Is(err, ErrAlreadyTerminal) {
		t.Fatalf("expected err to wrap ErrAlreadyTerminal, got: %v", err)
	}
	run, err := st.GetRun(ctx, "r")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if State(run.State) != StateCompleted {
		t.Fatalf("Cancel must not have touched the run's state; got %s", run.State)
	}
}

// cancelDuringComplete simulates the realistic SIGINT race: ctx is still
// alive when Step starts (its own GetRun succeeds, stepModel's
// ListMessages succeeds), and only becomes Done() partway through —
// exactly like a signal.NotifyContext-derived ctx being cancelled while
// the provider's HTTP request is in flight. Cancelling ctx unconditionally
// before returning an error models any provider failure that happens to
// coincide with cancellation, since what matters to stepModel is ctx.Err()
// on the ORIGINAL ctx, not the error text Complete returns.
type cancelDuringComplete struct {
	cancel context.CancelFunc
}

func (p cancelDuringComplete) Name() string { return "cancel-during-complete" }
func (p cancelDuringComplete) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	p.cancel()
	return nil, context.Canceled
}
func (p cancelDuringComplete) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	return nil, fmt.Errorf("cancelDuringComplete: Stream not implemented")
}
func (p cancelDuringComplete) Capabilities() provider.Capabilities { return provider.Capabilities{} }

// TestStepModelCancelledContextPersistsCancelled reproduces the bug this
// whole file is guarding against: a provider call fails because ctx was
// cancelled mid-flight (a SIGINT-derived ctx, an HTTP client disconnect).
// Before terminalCtx existed, stepModel tried to persist that failure
// using the same dead ctx, so the write failed too — the turn count was
// lost and the run stayed stuck in ready_for_model instead of reaching a
// terminal state. It should now reach StateCancelled specifically (not
// StateFailed), since ctx.Err() being context.Canceled is a reliable
// signal that something external stopped the run, not that the provider
// itself failed.
func TestStepModelCancelledContextPersistsCancelled(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	bg := context.Background()
	runCtx, cancel := context.WithCancel(bg)
	fp := cancelDuringComplete{cancel: cancel}
	eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10})

	if err := eng.NewRun(bg, "r", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	state, err := eng.Step(runCtx, "r")
	if err != nil {
		t.Fatalf("Step: %v (the terminal-state write itself must succeed even though ctx died mid-call)", err)
	}
	if state != StateCancelled {
		t.Fatalf("state = %s, want cancelled", state)
	}

	// And the write actually landed — confirmed on a fresh, live context,
	// not just trusted from Step's return value.
	run, err := st.GetRun(bg, "r")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if State(run.State) != StateCancelled {
		t.Fatalf("persisted state = %s, want cancelled", run.State)
	}
	if run.Error == nil || *run.Error != "run cancelled" {
		t.Fatalf("run.Error = %v, want \"run cancelled\"", run.Error)
	}
	// The turn count increment is exactly what a dead-ctx write used to
	// silently lose.
	if run.TurnCount != 1 {
		t.Fatalf("turn_count = %d, want 1", run.TurnCount)
	}
}

// waitForDoneThenComplete blocks until ctx is actually Done() (its
// deadline has really elapsed, not just a guessed sleep) and returns
// ctx.Err() — modeling a provider HTTP call aborted by a run-level
// deadline arriving mid-flight, the same way cancelDuringComplete models
// an explicit cancellation.
type waitForDoneThenComplete struct{}

func (waitForDoneThenComplete) Name() string { return "wait-for-done" }
func (waitForDoneThenComplete) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (waitForDoneThenComplete) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	return nil, fmt.Errorf("waitForDoneThenComplete: Stream not implemented")
}
func (waitForDoneThenComplete) Capabilities() provider.Capabilities { return provider.Capabilities{} }

// TestStepModelDeadlineExceededPersistsFailed proves the other half of
// the ctx.Err() branch: a deadline (limits.timeout, or any caller
// deadline) is a policy limit being hit, not an external stop request, so
// it belongs in StateFailed with a message that names it — not
// StateCancelled, which is reserved for an explicit cancellation.
func TestStepModelDeadlineExceededPersistsFailed(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	eng := NewEngine(st, waitForDoneThenComplete{}, Config{AgentName: "a", Model: "m", MaxTurns: 10})

	bg := context.Background()
	if err := eng.NewRun(bg, "r", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	deadlineCtx, cancel := context.WithTimeout(bg, 10*time.Millisecond)
	defer cancel()

	state, err := eng.Step(deadlineCtx, "r")
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != StateFailed {
		t.Fatalf("state = %s, want failed", state)
	}

	run, err := st.GetRun(bg, "r")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Error == nil || !strings.Contains(*run.Error, "exceeded its time limit") {
		t.Fatalf("run.Error = %v, want it to mention the time limit", run.Error)
	}
}
