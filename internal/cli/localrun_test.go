package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/message"
	"agentforge/internal/provider"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

// These exercise buildEngineFromStore and driveLocalRun directly — the
// two helpers `runs approve/deny/resume` (internal/cli/runs.go) and `run`
// (internal/cli/run.go) share — rather than going through cobra, mirroring
// how internal/api's tests exercise handlers directly instead of the
// `serve` command. scriptedProvider, toolUseResponse, and textResponse
// come from chat_model_test.go (same package).

const localRunTestYAML = `
name: test-agent
model:
  provider: ollama
  name: test-model
approvals:
  require:
    - "danger.tool"
limits:
  max_turns: 10
`

func fakeProviderFactory(p provider.Provider) agent.ProviderFactory {
	return func(config.ModelConfig) (provider.Provider, error) { return p, nil }
}

func newLocalRunTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedRun persists an agent + a run whose first message is already
// appended, mirroring what Engine.NewRun does internally — done directly
// against the store (not through an Engine) since buildEngineFromStore
// requires the run to already exist, and production code never has both
// an Engine and a not-yet-existing run at the same time (NewRun is called
// exactly once, before this reconstruction path is ever used).
func seedRun(t *testing.T, ctx context.Context, st *store.Store, runID string) {
	t.Helper()
	if err := st.UpsertAgent(ctx, "test-agent", localRunTestYAML); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	if err := st.CreateRun(ctx, runID, "test-agent", string(runtime.StateReadyForModel)); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.AppendMessage(ctx, runID, message.Text(message.RoleUser, "go")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
}

func newTestRegistry() *mcp.Registry {
	return mcp.NewRegistry(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestBuildEngineFromStoreDrivesToCompletion(t *testing.T) {
	ctx := context.Background()
	st := newLocalRunTestStore(t)
	seedRun(t, ctx, st, "run-complete")

	sp := &scriptedProvider{responses: []*provider.Response{textResponse("hi there")}}
	registry := newTestRegistry()
	defer registry.Close()

	eng, run, _, err := buildEngineFromStore(ctx, st, registry, "run-complete", fakeProviderFactory(sp))
	if err != nil {
		t.Fatalf("buildEngineFromStore: %v", err)
	}

	if err := driveLocalRun(ctx, st, eng, run.ID, outputOptions{}, false); err != nil {
		t.Fatalf("driveLocalRun: %v", err)
	}

	got, err := st.GetRun(ctx, "run-complete")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != string(runtime.StateCompleted) {
		t.Fatalf("expected completed, got %s (error=%v)", got.State, got.Error)
	}
}

// TestDriveLocalRunCancelledContextStopsCleanly reproduces what a real
// Ctrl-C during `agentforge run` does now that main.go wires ctx to
// SIGINT/SIGTERM: driveLocalRun sees ctx already done and must reach
// StateCancelled — not leave the run stuck in ready_for_model with a
// bare "context canceled" propagated to the caller. This is the fast
// pre-Step check (ctx dead before driveLocalRun's loop ever calls Step);
// internal/runtime's TestStepModelCancelledContextPersistsCancelled
// covers the other half — ctx dying mid-provider-call.
func TestDriveLocalRunCancelledContextStopsCleanly(t *testing.T) {
	ctx := context.Background()
	st := newLocalRunTestStore(t)
	seedRun(t, ctx, st, "run-cancel")

	sp := &scriptedProvider{responses: []*provider.Response{textResponse("should never be reached")}}
	registry := newTestRegistry()
	defer registry.Close()

	eng, run, _, err := buildEngineFromStore(ctx, st, registry, "run-cancel", fakeProviderFactory(sp))
	if err != nil {
		t.Fatalf("buildEngineFromStore: %v", err)
	}

	cancelledCtx, cancel := context.WithCancel(ctx)
	cancel()

	err = driveLocalRun(cancelledCtx, st, eng, run.ID, outputOptions{}, false)
	if err == nil {
		t.Fatal("expected driveLocalRun to return an error for a cancelled run")
	}
	if !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("expected the error to say the run was cancelled, got: %v", err)
	}

	// Confirmed on a fresh, live context — proving the state actually
	// persisted rather than just trusting driveLocalRun's return value.
	got, err := st.GetRun(ctx, "run-cancel")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != string(runtime.StateCancelled) {
		t.Fatalf("persisted state = %s, want cancelled", got.State)
	}
}

func TestBuildEngineFromStoreSurfacesPendingApprovalWithoutExecuting(t *testing.T) {
	ctx := context.Background()
	st := newLocalRunTestStore(t)
	seedRun(t, ctx, st, "run-pending")

	sp := &scriptedProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{"x":1}`),
		textResponse("done"),
	}}
	registry := newTestRegistry()
	defer registry.Close()

	eng, run, _, err := buildEngineFromStore(ctx, st, registry, "run-pending", fakeProviderFactory(sp))
	if err != nil {
		t.Fatalf("buildEngineFromStore: %v", err)
	}
	var executed bool
	eng.RegisterTool(runtime.Tool{
		Name: "danger.tool", InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			executed = true
			return "ok", nil
		},
	})

	if err := driveLocalRun(ctx, st, eng, run.ID, outputOptions{}, false); err != nil {
		t.Fatalf("driveLocalRun: %v", err)
	}

	got, err := st.GetRun(ctx, "run-pending")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != string(runtime.StateAwaitingApproval) {
		t.Fatalf("expected awaiting_approval, got %s", got.State)
	}
	if executed {
		t.Fatal("the tool must not execute before it's approved")
	}

	pending, err := st.ListPendingApprovals(ctx, "run-pending")
	if err != nil {
		t.Fatalf("ListPendingApprovals: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "call_1" || pending[0].ToolName != "danger.tool" {
		t.Fatalf("expected one pending call_1/danger.tool, got %+v", pending)
	}
}

func TestBuildEngineFromStoreFailedRunReturnsError(t *testing.T) {
	ctx := context.Background()
	st := newLocalRunTestStore(t)
	seedRun(t, ctx, st, "run-fail")

	sp := &scriptedProvider{responses: nil} // Complete errors immediately: no scripted responses
	registry := newTestRegistry()
	defer registry.Close()

	eng, run, _, err := buildEngineFromStore(ctx, st, registry, "run-fail", fakeProviderFactory(sp))
	if err != nil {
		t.Fatalf("buildEngineFromStore: %v", err)
	}

	if err := driveLocalRun(ctx, st, eng, run.ID, outputOptions{}, false); err == nil {
		t.Fatal("expected driveLocalRun to return a non-nil error for a failed run")
	}
}

// TestBuildEngineFromStoreRejectsPlaceholderAgent reproduces the resume
// bug this test guards against: a run whose agent row was created by
// Engine.NewRun's EnsureAgentExists (empty-YAML placeholder, to satisfy
// runs.agent_name's foreign key) rather than a real UpsertAgent — every
// current CLI path upserts real config first, so this can't happen via
// `run`/`chat` today, but a direct runtime.Engine caller (a future SDK,
// or a test) could hit it. Before the fix, this failed with
// config.Parse("")'s "name is required", which reads like a corrupted
// YAML file rather than what actually happened.
func TestBuildEngineFromStoreRejectsPlaceholderAgent(t *testing.T) {
	ctx := context.Background()
	st := newLocalRunTestStore(t)

	if err := st.EnsureAgentExists(ctx, "placeholder-agent"); err != nil {
		t.Fatalf("EnsureAgentExists: %v", err)
	}
	if err := st.CreateRun(ctx, "run-placeholder", "placeholder-agent", string(runtime.StateReadyForModel)); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.AppendMessage(ctx, "run-placeholder", message.Text(message.RoleUser, "go")); err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}

	registry := newTestRegistry()
	defer registry.Close()

	_, _, _, err := buildEngineFromStore(ctx, st, registry, "run-placeholder", fakeProviderFactory(&scriptedProvider{}))
	if err == nil {
		t.Fatal("expected buildEngineFromStore to reject a placeholder (empty-YAML) agent")
	}
	if !strings.Contains(err.Error(), "has no stored config") {
		t.Fatalf("expected a clear \"has no stored config\" error, got: %v", err)
	}
}

func TestApproveThenDriveExecutesAndCompletes(t *testing.T) {
	ctx := context.Background()
	st := newLocalRunTestStore(t)
	seedRun(t, ctx, st, "run-approve")

	sp := &scriptedProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{"x":1}`),
		textResponse("done"),
	}}
	registry := newTestRegistry()
	defer registry.Close()

	eng, run, _, err := buildEngineFromStore(ctx, st, registry, "run-approve", fakeProviderFactory(sp))
	if err != nil {
		t.Fatalf("buildEngineFromStore: %v", err)
	}
	var executed bool
	eng.RegisterTool(runtime.Tool{
		Name: "danger.tool", InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			executed = true
			return "ok", nil
		},
	})
	if err := driveLocalRun(ctx, st, eng, run.ID, outputOptions{}, false); err != nil {
		t.Fatalf("driveLocalRun (first pass): %v", err)
	}

	// This is the crux of `runs approve`: RecordApproval, then drive the
	// same engine forward again.
	if _, err := eng.RecordApproval(ctx, run.ID, "call_1", "approved", "cli", ""); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	if err := driveLocalRun(ctx, st, eng, run.ID, outputOptions{}, false); err != nil {
		t.Fatalf("driveLocalRun (after approval): %v", err)
	}

	if !executed {
		t.Fatal("expected the approved tool to have executed")
	}
	got, err := st.GetRun(ctx, "run-approve")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.State != string(runtime.StateCompleted) {
		t.Fatalf("expected completed, got %s (error=%v)", got.State, got.Error)
	}
}
