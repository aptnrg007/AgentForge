package cli

import (
	"context"
	"fmt"
	"time"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

// buildEngineFromStore reconstructs the engine that owns runID from its
// agent's persisted config — the CLI-side equivalent of the HTTP daemon's
// buildEngineForRun (internal/api/handlers.go). Unlike `run`/`chat`, the
// `runs` subcommands (approve/deny/resume) only have a run ID, not a
// config file path, so the agent's YAML has to come back out of the
// store. Takes a ProviderFactory (rather than always using
// agent.DefaultProviderFactory) so tests can inject a fake instead of
// hitting a real Ollama. Also returns the parsed *config.Config — the
// caller needs cfg.Output.Schema != "" (whether this run's config sets a
// schema) to decide the JSON envelope's `output` type, and reconstructing
// it a second time would mean parsing the same YAML twice.
func buildEngineFromStore(ctx context.Context, st *store.Store, registry *mcp.Registry, runID string, pf agent.ProviderFactory) (*runtime.Engine, *store.Run, *config.Config, error) {
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		return nil, nil, nil, err
	}
	ag, err := st.GetAgent(ctx, run.AgentName)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err := config.Parse([]byte(ag.YAML))
	if err != nil {
		return nil, nil, nil, err
	}
	eng, err := agent.Build(ctx, st, registry, cfg, pf)
	if err != nil {
		return nil, nil, nil, err
	}
	return eng, run, cfg, nil
}

// driveLocalRun steps eng until runID reaches a stop point (completed,
// failed, cancelled, or awaiting_approval), then emits the outcome in the
// requested format. It returns a non-nil error for a failed run instead
// of only printing to stderr, so a script chaining on `agentforge
// run`/`runs resume` sees a non-zero exit code when the run actually
// failed. schemaSet is the owning agent's cfg.Output.Schema != "" — see
// buildEngineFromStore — used only to decide the JSON envelope's `output`
// type; it has no effect on text-mode output.
func driveLocalRun(ctx context.Context, st *store.Store, eng *runtime.Engine, runID string, o outputOptions, schemaSet bool) error {
	start := time.Now()

	for {
		state, err := eng.Step(ctx, runID)
		if err != nil {
			return err
		}

		switch state {
		case runtime.StateAwaitingApproval:
			pending, err := st.ListPendingApprovals(ctx, runID)
			if err != nil {
				return err
			}
			pendingRows := make([]remotePendingCall, len(pending))
			for i, tc := range pending {
				pendingRows[i] = remotePendingCall{CallID: tc.ID, Tool: tc.ToolName, Args: []byte(tc.ArgsJSON)}
			}
			return emitOutcome(o, runResult{
				RunID: runID, State: string(state), Pending: pendingRows,
				DurationMS: time.Since(start).Milliseconds(), SchemaSet: schemaSet,
			})

		case runtime.StateCompleted:
			msgs, err := st.ListMessages(ctx, runID)
			if err != nil {
				return err
			}
			return emitOutcome(o, runResult{
				RunID: runID, State: string(state), Messages: msgs,
				DurationMS: time.Since(start).Milliseconds(), SchemaSet: schemaSet,
			})

		case runtime.StateFailed:
			msgs, mErr := st.ListMessages(ctx, runID)
			run, gErr := st.GetRun(ctx, runID)
			if gErr != nil {
				return gErr
			}
			errStr := "unknown error"
			if run.Error != nil {
				errStr = *run.Error
			}
			if mErr == nil {
				if err := emitOutcome(o, runResult{
					RunID: runID, State: string(state), Messages: msgs, Error: &errStr,
					DurationMS: time.Since(start).Milliseconds(), SchemaSet: schemaSet,
				}); err != nil {
					return err
				}
			}
			return fmt.Errorf("run %s failed: %s", runID, errStr)

		case runtime.StateCancelled:
			return fmt.Errorf("run %s was cancelled", runID)
		}
		// ready_for_model / ready_for_tools: keep stepping.
	}
}

// emitOutcome resolves --output's target (stdout or a file) and writes
// res to it in the requested format, closing the target afterward.
func emitOutcome(o outputOptions, res runResult) error {
	w, closeW, err := outputTarget(o)
	if err != nil {
		return err
	}
	defer closeW()
	return emitRunResult(w, o, res)
}
