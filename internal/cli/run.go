package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/message"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

func newRunCmd() *cobra.Command {
	var (
		dbPath    string
		msg       string
		server    string
		authToken string
		out       outputOptions
	)

	cmd := &cobra.Command{
		Use:   "run <agent.yaml>",
		Short: "Run an agent once and print the result",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if msg == "" {
				return fmt.Errorf("-m/--message is required")
			}
			if err := out.validate(); err != nil {
				return err
			}
			resolvedMsg, err := resolveMessage(msg)
			if err != nil {
				return err
			}
			if server != "" {
				return runRemote(cmd.Context(), server, authToken, args[0], resolvedMsg, out)
			}
			return runLocal(cmd.Context(), dbPath, args[0], resolvedMsg, out)
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", defaultDBPath(), "path to the SQLite run store (embedded mode only)")
	cmd.Flags().StringVarP(&msg, "message", "m", "", `message to send the agent; "@path" reads it from a file`)
	cmd.Flags().StringVar(&server, "server", "", "daemon URL to run against instead of an embedded engine, e.g. http://localhost:8080")
	cmd.Flags().StringVar(&authToken, "auth-token", defaultAuthToken(), "bearer token for --server, if it was started with its own --auth-token (default: $AGENTFORGE_AUTH_TOKEN)")
	addOutputFlags(cmd, &out)
	return cmd
}

// runLocal drives the agent with an embedded engine: no daemon involved.
// It still upserts the agent into the local store so `agentforge agents
// list/get` (without --server) can see agents that were run this way.
func runLocal(ctx context.Context, dbPath, cfgPath, msg string, o outputOptions) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	rawYAML, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}

	if err := ensureDBDir(dbPath); err != nil {
		return err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.UpsertAgent(ctx, cfg.Name, string(rawYAML)); err != nil {
		return err
	}

	registry := mcp.NewRegistry(slog.Default())
	defer registry.Close()

	eng, err := agent.Build(ctx, st, registry, cfg, agent.DefaultProviderFactory)
	if err != nil {
		return err
	}

	runID := newRunID()
	if err := eng.NewRun(ctx, runID, msg); err != nil {
		return err
	}
	fmt.Fprintf(progressWriter(o), "run %s started\n", runID)

	return driveLocalRun(ctx, st, eng, runID, o, cfg.Output.Schema != "")
}

// runRemote registers the agent with a running daemon and drives the run
// there over its SSE endpoint (POST .../stream) instead of the atomic
// POST .../run — the --server counterpart to driveLocalRun's live
// token/tool_call/tool_result output, so text-mode output looks the same
// whether the run happened in-process or against a daemon. Unlike the
// local path, the run ID isn't known until the stream's terminal frame
// (the daemon generates it server-side), so there's no "run X started"
// line to print up front the way runLocal has — the streamed output
// itself is the progress signal here.
func runRemote(ctx context.Context, server, authToken, cfgPath, msg string, o outputOptions) error {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}

	var ag remoteAgent
	if err := apiPost(ctx, server+"/v1/agents", "text/yaml", raw, authToken, &ag); err != nil {
		return fmt.Errorf("register agent: %w", err)
	}

	reqBody, err := json.Marshal(map[string]string{"message": msg})
	if err != nil {
		return err
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server+"/v1/agents/"+ag.Name+"/stream", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("stream agent: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRemoteErrBodyBytes))
		var eb apiErrorBody
		if json.Unmarshal(body, &eb) == nil && eb.Error != "" {
			return fmt.Errorf("stream agent: server returned %d: %s", resp.StatusCode, eb.Error)
		}
		return fmt.Errorf("stream agent: server returned %d: %s", resp.StatusCode, string(body))
	}

	ep := newEventPrinter(o)
	var (
		runID   string
		state   string
		errStr  *string
		pending []remotePendingCall
	)
	scanErr := readSSE(resp.Body, func(ev sseEvent) bool {
		switch ev.Event {
		case "token":
			var payload struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(ev.Data, &payload) == nil {
				ep.onEvent(runtime.Event{Kind: runtime.EventToken, Text: payload.Text})
			}
		case "tool_call":
			var payload struct {
				Tool string          `json:"tool"`
				Args json.RawMessage `json:"args"`
			}
			if json.Unmarshal(ev.Data, &payload) == nil {
				ep.onEvent(runtime.Event{Kind: runtime.EventToolCall, ToolName: payload.Tool, Args: payload.Args})
			}
		case "tool_result":
			var payload struct {
				Tool    string `json:"tool"`
				Result  string `json:"result"`
				IsError bool   `json:"is_error"`
			}
			if json.Unmarshal(ev.Data, &payload) == nil {
				ep.onEvent(runtime.Event{Kind: runtime.EventToolResult, ToolName: payload.Tool, Result: payload.Result, IsError: payload.IsError})
			}
		case "awaiting_approval":
			var payload struct {
				RunID   string              `json:"run_id"`
				Pending []remotePendingCall `json:"pending"`
			}
			if json.Unmarshal(ev.Data, &payload) == nil {
				runID, state, pending = payload.RunID, "awaiting_approval", payload.Pending
			}
			return true
		case "done":
			var payload struct {
				RunID string  `json:"run_id"`
				State string  `json:"state"`
				Error *string `json:"error,omitempty"`
			}
			if json.Unmarshal(ev.Data, &payload) == nil {
				runID, state, errStr = payload.RunID, payload.State, payload.Error
			}
			return true
		}
		return false
	})
	ep.endLine()
	if scanErr != nil {
		return fmt.Errorf("stream agent: %w", scanErr)
	}
	if runID == "" {
		return fmt.Errorf("stream agent: connection closed before a run outcome arrived")
	}

	// The stream itself only carries tokens and tool events, not the
	// run's full message history — fetch that the same way `runs get`
	// does, so the streamed and (still atomic) resume/approve/deny
	// --server paths report identical tool_calls_count/output in JSON
	// mode.
	var trace remoteRunTrace
	if err := apiGet(ctx, server+"/v1/runs/"+runID, authToken, &trace); err != nil {
		return fmt.Errorf("fetch run %s: %w", runID, err)
	}
	msgs := make([]message.Message, len(trace.Messages))
	for i, m := range trace.Messages {
		msgs[i] = message.Message{Role: m.Role, Content: m.Content}
	}

	run := remoteRun{
		RunID: runID, State: state, Error: errStr, Messages: msgs,
		Pending: pending, Resumable: state == "interrupted",
	}
	return emitRemoteRunOutcome(run, o, server, schemaSetFromYAML(raw), time.Since(start), true)
}

// schemaSetFromYAML reports whether the config the CLI is about to send
// to the daemon sets output.schema — used only to pick the remote JSON
// envelope's `output` type (object vs. string), the same signal
// buildEngineFromStore's *config.Config return gives the local path. A
// parse failure here can't happen in practice (apiPost above would have
// already rejected the same bytes when registering the agent), so it's
// treated as "no schema" rather than surfaced as a second error path.
func schemaSetFromYAML(raw []byte) bool {
	cfg, err := config.Parse(raw)
	if err != nil {
		return false
	}
	return cfg.Output.Schema != ""
}

// emitRemoteRunOutcome renders a remoteRun's result and reports whether
// the run failed, so callers can propagate a real exit code instead of
// always returning nil regardless of outcome.
// A --output file's Close error is checked, not dropped — same reasoning
// as emitOutcome in localrun.go — but only surfaces when nothing else
// already failed: a run that genuinely failed should still report *that*
// error, not a Close error that raced it.
func emitRemoteRunOutcome(run remoteRun, o outputOptions, server string, schemaSet bool, elapsed time.Duration, streamed bool) (err error) {
	res := runResult{
		RunID: run.RunID, State: run.State, Error: run.Error, Messages: run.Messages,
		Pending: run.Pending, DurationMS: elapsed.Milliseconds(), SchemaSet: schemaSet, Server: server,
		Resumable: run.Resumable, Streamed: streamed,
	}
	w, closeW, err := outputTarget(o)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := closeW(); cerr != nil && err == nil {
			err = fmt.Errorf("write --output %s: %w", o.path, cerr)
		}
	}()
	if err = emitRunResult(w, o, res); err != nil {
		return err
	}

	switch run.State {
	case "failed":
		errStr := "unknown error"
		if run.Error != nil {
			errStr = *run.Error
		}
		err = fmt.Errorf("run %s failed: %s", run.RunID, errStr)
	case "interrupted":
		errStr := "unknown error"
		if run.Error != nil {
			errStr = *run.Error
		}
		err = fmt.Errorf("run %s was interrupted: %s (resume with: agentforge runs resume %s --server %s)", run.RunID, errStr, run.RunID, server)
	}
	return err
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("run_%x", b)
}
