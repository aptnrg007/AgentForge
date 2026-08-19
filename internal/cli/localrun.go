package cli

import (
	"context"
	"fmt"

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
// hitting a real Ollama.
func buildEngineFromStore(ctx context.Context, st *store.Store, registry *mcp.Registry, runID string, pf agent.ProviderFactory) (*runtime.Engine, *store.Run, error) {
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	ag, err := st.GetAgent(ctx, run.AgentName)
	if err != nil {
		return nil, nil, err
	}
	cfg, err := config.Parse([]byte(ag.YAML))
	if err != nil {
		return nil, nil, err
	}
	eng, err := agent.Build(ctx, st, registry, cfg, pf)
	if err != nil {
		return nil, nil, err
	}
	return eng, run, nil
}

// driveLocalRun steps eng until runID reaches a stop point (completed,
// failed, cancelled, or awaiting_approval), printing progress as it goes.
// It returns a non-nil error for a failed run instead of only printing to
// stderr, so a script chaining on `agentforge run`/`runs resume` sees a
// non-zero exit code when the run actually failed.
func driveLocalRun(ctx context.Context, st *store.Store, eng *runtime.Engine, runID string) error {
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
			fmt.Printf("run %s is awaiting approval:\n", runID)
			for _, tc := range pending {
				fmt.Printf("  %s  %s(%s)\n", tc.ID, tc.ToolName, tc.ArgsJSON)
			}
			fmt.Printf("decide with: agentforge runs approve|deny %s <call-id>\n", runID)
			return nil

		case runtime.StateCompleted:
			msgs, err := st.ListMessages(ctx, runID)
			if err != nil {
				return err
			}
			printMessages(msgs)
			return nil

		case runtime.StateFailed:
			msgs, mErr := st.ListMessages(ctx, runID)
			if mErr == nil {
				printMessages(msgs)
			}
			run, err := st.GetRun(ctx, runID)
			if err != nil {
				return err
			}
			errStr := "unknown error"
			if run.Error != nil {
				errStr = *run.Error
			}
			return fmt.Errorf("run %s failed: %s", runID, errStr)

		case runtime.StateCancelled:
			return fmt.Errorf("run %s was cancelled", runID)
		}
		// ready_for_model / ready_for_tools: keep stepping.
	}
}
