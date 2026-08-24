package cli

import (
	"context"
	"path/filepath"
	"testing"

	"agentforge/internal/message"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

// TestRunsCancelStopsNonTerminalRun exercises runsCancel's local-store
// path directly (server == ""). Unlike buildEngineFromStore-backed
// commands (approve/deny/resume), runsCancel opens its own *store.Store
// from dbPath rather than reusing an already-open one, mirroring how a
// real CLI invocation works — so this seeds the run through a throwaway
// store.Open/Close instead of newLocalRunTestStore.
func TestRunsCancelStopsNonTerminalRun(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.UpsertAgent(ctx, "test-agent", localRunTestYAML); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	if err := st.CreateRun(ctx, "run-1", "test-agent", string(runtime.StateReadyForModel)); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.AppendMessage(ctx, "run-1", message.Text(message.RoleUser, "go")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	if err := runsCancel(ctx, "", dbPath, "run-1"); err != nil {
		t.Fatalf("runsCancel: %v", err)
	}

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st2.Close()
	run, err := st2.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if runtime.State(run.State) != runtime.StateCancelled {
		t.Fatalf("state = %s, want cancelled", run.State)
	}
}

// TestRunsCancelAlreadyTerminalReturnsError proves cancelling a run
// that's already completed exits with a non-zero-worthy error (Execute
// prints "error: ..." and os.Exit(1) sees a non-nil return) rather than
// silently succeeding.
func TestRunsCancelAlreadyTerminalReturnsError(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.UpsertAgent(ctx, "test-agent", localRunTestYAML); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	if err := st.CreateRun(ctx, "run-done", "test-agent", string(runtime.StateCompleted)); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	if err := runsCancel(ctx, "", dbPath, "run-done"); err == nil {
		t.Fatal("expected an error cancelling an already-completed run")
	}
}
