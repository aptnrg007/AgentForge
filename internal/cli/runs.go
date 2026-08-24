package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"agentforge/internal/agent"
	"agentforge/internal/mcp"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

func newRunsCmd() *cobra.Command {
	var server, dbPath, authToken string
	var agentFilter string
	var limit int
	var reason string
	var approveOut, denyOut, resumeOut outputOptions

	list := &cobra.Command{
		Use:   "list",
		Short: "List runs, most recent first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runsList(cmd.Context(), server, dbPath, authToken, agentFilter, limit)
		},
	}
	list.Flags().StringVar(&agentFilter, "agent", "", "only show runs for this agent")
	list.Flags().IntVar(&limit, "limit", 20, "maximum number of runs to show")

	get := &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show a run's full trace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runsGet(cmd.Context(), server, dbPath, authToken, args[0])
		},
	}

	approve := &cobra.Command{
		Use:   "approve <run-id> <call-id>",
		Short: "Approve a pending tool call and continue the run",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := approveOut.validate(); err != nil {
				return err
			}
			return runsDecide(cmd.Context(), server, dbPath, authToken, args[0], args[1], "approved", reason, approveOut)
		},
	}
	deny := &cobra.Command{
		Use:   "deny <run-id> <call-id>",
		Short: "Deny a pending tool call and continue the run",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := denyOut.validate(); err != nil {
				return err
			}
			return runsDecide(cmd.Context(), server, dbPath, authToken, args[0], args[1], "denied", reason, denyOut)
		},
	}
	approve.Flags().StringVar(&reason, "reason", "", "reason recorded alongside the decision")
	deny.Flags().StringVar(&reason, "reason", "", "reason recorded alongside the decision")
	addOutputFlags(approve, &approveOut)
	addOutputFlags(deny, &denyOut)

	resume := &cobra.Command{
		Use:   "resume <run-id>",
		Short: "Continue a run whose pending calls have already been decided",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := resumeOut.validate(); err != nil {
				return err
			}
			return runsResume(cmd.Context(), server, dbPath, authToken, args[0], resumeOut)
		},
	}
	addOutputFlags(resume, &resumeOut)

	cancel := &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Stop a non-terminal run immediately",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runsCancel(cmd.Context(), server, dbPath, authToken, args[0])
		},
	}

	var statsAgent string
	stats := &cobra.Command{
		Use:   "stats",
		Short: "Aggregate success rate, turns, tool calls, and token spend across runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runsStats(cmd.Context(), server, dbPath, statsAgent)
		},
	}
	stats.Flags().StringVar(&statsAgent, "agent", "", "only aggregate runs for this agent")

	cmd := &cobra.Command{Use: "runs", Short: "Inspect and drive runs"}
	cmd.PersistentFlags().StringVar(&server, "server", "", "daemon URL, e.g. http://localhost:8080 (defaults to the local --db store)")
	cmd.PersistentFlags().StringVar(&dbPath, "db", defaultDBPath(), "path to the SQLite run store (used when --server is not set)")
	cmd.PersistentFlags().StringVar(&authToken, "auth-token", defaultAuthToken(), "bearer token for --server, if it was started with its own --auth-token (default: $AGENTFORGE_AUTH_TOKEN)")
	cmd.AddCommand(list, get, approve, deny, resume, cancel, stats)
	return cmd
}

func runsList(ctx context.Context, server, dbPath, authToken, agentFilter string, limit int) error {
	if server != "" {
		url := server + "/v1/runs?limit=" + strconv.Itoa(limit)
		if agentFilter != "" {
			url += "&agent=" + agentFilter
		}
		var runs []remoteRunSummary
		if err := apiGet(ctx, url, authToken, &runs); err != nil {
			return err
		}
		rows := make([]runRow, len(runs))
		for i, r := range runs {
			rows[i] = runRow{ID: r.RunID, AgentName: r.AgentName, State: r.State, TurnCount: r.TurnCount, CreatedAt: r.CreatedAt}
		}
		printRunsList(rows)
		return nil
	}

	if err := ensureDBDir(dbPath); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	runs, err := st.ListRuns(ctx, agentFilter, limit)
	if err != nil {
		return err
	}
	rows := make([]runRow, len(runs))
	for i, r := range runs {
		rows[i] = runRow{ID: r.ID, AgentName: r.AgentName, State: r.State, TurnCount: r.TurnCount, CreatedAt: r.CreatedAt}
	}
	printRunsList(rows)
	return nil
}

func runsGet(ctx context.Context, server, dbPath, authToken, id string) error {
	if server != "" {
		var trace remoteRunTrace
		if err := apiGet(ctx, server+"/v1/runs/"+id, authToken, &trace); err != nil {
			return err
		}
		calls := make([]toolCallRow, len(trace.ToolCalls))
		for i, tc := range trace.ToolCalls {
			calls[i] = toolCallRow{ID: tc.ID, ToolName: tc.ToolName, Approval: tc.Approval, DecidedBy: tc.DecidedBy, Reason: tc.Reason, Result: tc.Result, IsError: tc.IsError, DurationMS: tc.DurationMS}
		}
		msgs := make([]messageRow, len(trace.Messages))
		for i, m := range trace.Messages {
			msgs[i] = messageRow(m)
		}
		printRunTrace(trace.RunID, trace.State, trace.TurnCount, trace.Error, msgs, calls)
		return nil
	}

	if err := ensureDBDir(dbPath); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	run, err := st.GetRun(ctx, id)
	if err != nil {
		return err
	}
	details, err := st.ListMessagesDetailed(ctx, id)
	if err != nil {
		return err
	}
	toolCalls, err := st.ListToolCalls(ctx, id)
	if err != nil {
		return err
	}
	calls := make([]toolCallRow, len(toolCalls))
	for i, tc := range toolCalls {
		calls[i] = toolCallRow{ID: tc.ID, ToolName: tc.ToolName, Approval: tc.Approval, DecidedBy: tc.DecidedBy, Reason: tc.Reason, Result: tc.Result, IsError: tc.IsError, DurationMS: tc.DurationMS}
	}
	msgs := make([]messageRow, len(details))
	for i, d := range details {
		msgs[i] = messageRow{Role: d.Role, Content: d.Content, InputTokens: d.InputTokens, OutputTokens: d.OutputTokens, LatencyMS: d.LatencyMS}
	}
	printRunTrace(run.ID, run.State, run.TurnCount, run.Error, msgs, calls)
	return nil
}

// runsDecide records an approve/deny decision on one pending call, then
// continues driving the run — the CLI path the README's approval-gate
// story previously had no way to complete outside `agentforge chat`.
func runsDecide(ctx context.Context, server, dbPath, authToken, runID, callID, decision, reason string, o outputOptions) error {
	if server != "" {
		reqBody, err := json.Marshal(map[string]string{"call_id": callID, "decision": decision, "reason": reason})
		if err != nil {
			return err
		}
		var result map[string]string
		if err := apiPost(ctx, server+"/v1/runs/"+runID+"/approve", "application/json", reqBody, authToken, &result); err != nil {
			return err
		}
		fmt.Fprintf(progressWriter(o), "%s: %s\n", decision, callID)
		start := time.Now()
		var run remoteRun
		if err := apiPost(ctx, server+"/v1/runs/"+runID+"/resume", "application/json", nil, authToken, &run); err != nil {
			return err
		}
		return emitRemoteRunOutcome(run, o, server, false, time.Since(start))
	}

	if err := ensureDBDir(dbPath); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	registry := mcp.NewRegistry(slog.Default())
	defer registry.Close()

	eng, run, cfg, err := buildEngineFromStore(ctx, st, registry, runID, agent.DefaultProviderFactory)
	if err != nil {
		return err
	}
	if _, err := eng.RecordApproval(ctx, run.ID, callID, decision, "cli", reason); err != nil {
		return err
	}
	fmt.Fprintf(progressWriter(o), "%s: %s\n", decision, callID)

	return driveLocalRun(ctx, st, eng, run.ID, o, cfg.Output.Schema != "")
}

func runsResume(ctx context.Context, server, dbPath, authToken, runID string, o outputOptions) error {
	if server != "" {
		start := time.Now()
		var run remoteRun
		if err := apiPost(ctx, server+"/v1/runs/"+runID+"/resume", "application/json", nil, authToken, &run); err != nil {
			return err
		}
		return emitRemoteRunOutcome(run, o, server, false, time.Since(start))
	}

	if err := ensureDBDir(dbPath); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	registry := mcp.NewRegistry(slog.Default())
	defer registry.Close()

	eng, run, cfg, err := buildEngineFromStore(ctx, st, registry, runID, agent.DefaultProviderFactory)
	if err != nil {
		return err
	}
	return driveLocalRun(ctx, st, eng, run.ID, o, cfg.Output.Schema != "")
}

// runsCancel stops a non-terminal run. Deliberately doesn't reconstruct a
// real engine via buildEngineFromStore the way approve/deny/resume do —
// runtime.Engine.Cancel only ever touches persisted run state
// (store.GetRun/UpdateRun), never the provider or tools, so it doesn't
// need one — and a stuck run behind a since-broken agent config (bad
// YAML, a revoked API key) should still be cancellable; that's the whole
// point of an escape hatch. Mirrors internal/api/handlers.go's
// handleCancel, which makes the same call for the same reason.
func runsCancel(ctx context.Context, server, dbPath, authToken, runID string) error {
	if server != "" {
		var result map[string]string
		if err := apiPost(ctx, server+"/v1/runs/"+runID+"/cancel", "application/json", nil, authToken, &result); err != nil {
			return err
		}
		fmt.Printf("%s: %s\n", result["state"], runID)
		return nil
	}

	if err := ensureDBDir(dbPath); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	eng := runtime.NewEngine(st, nil, runtime.Config{})
	state, err := eng.Cancel(ctx, runID)
	if err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", state, runID)
	return nil
}

// runsStats aggregates local run history — the daemon has no equivalent
// endpoint yet, so --server is rejected explicitly rather than silently
// ignored (which would otherwise look like it aggregated the daemon's
// runs when it actually read the local --db file instead).
func runsStats(ctx context.Context, server, dbPath, agentFilter string) error {
	if server != "" {
		return fmt.Errorf("runs stats does not support --server yet; run it against the daemon's --db file directly")
	}

	if err := ensureDBDir(dbPath); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	stats, err := st.Stats(ctx, agentFilter)
	if err != nil {
		return err
	}
	printStats(stats, agentFilter)
	return nil
}
