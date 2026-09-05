package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

// eventPrinter renders an Engine's fine-grained progress events as a live
// text stream — token deltas as they're generated, plus a line per tool
// call/result — instead of the pre-streaming CLI's one atomic block of
// output at the run's next stop point. inText tracks whether the cursor is
// mid token-stream (no trailing newline yet), so a tool_call/tool_result
// line — or the caller's final endLine — knows whether it needs to close
// that line first.
type eventPrinter struct {
	w      io.Writer
	inText bool
}

func newEventPrinter(o outputOptions) *eventPrinter {
	return &eventPrinter{w: progressWriter(o)}
}

func (p *eventPrinter) onEvent(ev runtime.Event) {
	switch ev.Kind {
	case runtime.EventToken:
		if !p.inText {
			fmt.Fprint(p.w, "agent: ")
			p.inText = true
		}
		fmt.Fprint(p.w, ev.Text)
	case runtime.EventToolCall:
		p.endLine()
		fmt.Fprintf(p.w, "agent: calling %s(%s)\n", ev.ToolName, string(ev.Args))
	case runtime.EventToolResult:
		p.endLine()
		label := "tool_result"
		if ev.IsError {
			label = "tool_error"
		}
		fmt.Fprintf(p.w, "%s[%s]: %s\n", label, ev.ToolName, ev.Result)
	}
}

// endLine closes off a token stream left without a trailing newline — a
// no-op if the last event already did (a tool_call/tool_result) or nothing
// has streamed yet.
func (p *eventPrinter) endLine() {
	if p.inText {
		fmt.Fprintln(p.w)
		p.inText = false
	}
}

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
	if ag.YAML == "" {
		// See the matching check in internal/api/handlers.go
		// buildEngineForRun: this run's agent row is a placeholder
		// (EnsureAgentExists) rather than real config, which shouldn't
		// happen via any current CLI path — `run`/`chat` upsert real YAML
		// before starting a run — but a bare config.Parse("") failure
		// ("name is required") would misleadingly look like the stored
		// YAML itself is broken.
		return nil, nil, nil, fmt.Errorf("agent %q has no stored config (run %s cannot be resumed)", run.AgentName, runID)
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

	ep := newEventPrinter(o)
	eng.OnEvent(ep.onEvent)

	state, err := eng.Run(ctx, runID)
	ep.endLine()
	if err != nil {
		if ctx.Err() != nil {
			// eng.Run hit a dead ctx (Ctrl-C via main.go's signal-derived
			// ctx, or limits.timeout) rather than a genuine failure —
			// state is whatever EndIfContextDone determined the run ended
			// up in. Reported directly here rather than through the
			// switch below, which would read the run's trace using this
			// same (dead) ctx to build the full emitOutcome payload;
			// there's nothing new to report beyond "it stopped and why"
			// in this case anyway — `runs get <id>` can inspect the rest
			// afterward with a live context.
			if state == runtime.StateFailed {
				return fmt.Errorf("run %s failed: %s", runID, ctx.Err())
			}
			return fmt.Errorf("run %s was cancelled", runID) // StateCancelled, or "" if even EndIfContextDone couldn't tell
		}
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
			DurationMS: time.Since(start).Milliseconds(), SchemaSet: schemaSet, Streamed: true,
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
				DurationMS: time.Since(start).Milliseconds(), SchemaSet: schemaSet, Streamed: true,
			}); err != nil {
				return err
			}
		}
		return fmt.Errorf("run %s failed: %s", runID, errStr)

	case runtime.StateCancelled:
		return fmt.Errorf("run %s was cancelled", runID)

	case runtime.StateInterrupted:
		// Unlike awaiting_approval, there's nothing to decide first — the
		// run just needs a later `runs resume` once whatever it hit (a
		// rate limit, an outage) has had a chance to clear. Still a
		// non-zero exit, so a script chaining on `agentforge run` notices,
		// but a distinguishable message and error shape from a genuine
		// failure.
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
				DurationMS: time.Since(start).Milliseconds(), SchemaSet: schemaSet, Resumable: true, Streamed: true,
			}); err != nil {
				return err
			}
		}
		return fmt.Errorf("run %s was interrupted: %s (resume with: agentforge runs resume %s)", runID, errStr, runID)
	}
	return fmt.Errorf("run %s: unexpected state %q", runID, state)
}

// emitOutcome resolves --output's target (stdout or a file) and writes
// res to it in the requested format, closing the target afterward. A
// --output file's Close error is checked (not just deferred and dropped):
// a full disk can make the write look like it succeeded and only surface
// on flush, and silently producing a truncated JSON envelope for a script
// to consume is worse than a clear error.
func emitOutcome(o outputOptions, res runResult) (err error) {
	w, closeW, err := outputTarget(o)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := closeW(); cerr != nil && err == nil {
			err = fmt.Errorf("write --output %s: %w", o.path, cerr)
		}
	}()
	return emitRunResult(w, o, res)
}
