package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/store"
)

func newRunCmd() *cobra.Command {
	var (
		dbPath string
		msg    string
		server string
		out    outputOptions
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
				return runRemote(cmd.Context(), server, args[0], resolvedMsg, out)
			}
			return runLocal(cmd.Context(), dbPath, args[0], resolvedMsg, out)
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", defaultDBPath(), "path to the SQLite run store (embedded mode only)")
	cmd.Flags().StringVarP(&msg, "message", "m", "", `message to send the agent; "@path" reads it from a file`)
	cmd.Flags().StringVar(&server, "server", "", "daemon URL to run against instead of an embedded engine, e.g. http://localhost:8080")
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

// runRemote registers the agent with a running daemon and runs it there.
func runRemote(ctx context.Context, server, cfgPath, msg string, o outputOptions) error {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}

	var ag remoteAgent
	if err := apiPost(ctx, server+"/v1/agents", "text/yaml", raw, &ag); err != nil {
		return fmt.Errorf("register agent: %w", err)
	}

	reqBody, err := json.Marshal(map[string]string{"message": msg})
	if err != nil {
		return err
	}
	start := time.Now()
	var run remoteRun
	if err := apiPost(ctx, server+"/v1/agents/"+ag.Name+"/run", "application/json", reqBody, &run); err != nil {
		return fmt.Errorf("run agent: %w", err)
	}

	fmt.Fprintf(progressWriter(o), "run %s started\n", run.RunID)
	return emitRemoteRunOutcome(run, o, server, schemaSetFromYAML(raw), time.Since(start))
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
func emitRemoteRunOutcome(run remoteRun, o outputOptions, server string, schemaSet bool, elapsed time.Duration) error {
	res := runResult{
		RunID: run.RunID, State: run.State, Error: run.Error, Messages: run.Messages,
		Pending: run.Pending, DurationMS: elapsed.Milliseconds(), SchemaSet: schemaSet, Server: server,
	}
	w, closeW, err := outputTarget(o)
	if err != nil {
		return err
	}
	defer closeW()
	if err := emitRunResult(w, o, res); err != nil {
		return err
	}

	if run.State == "failed" {
		errStr := "unknown error"
		if run.Error != nil {
			errStr = *run.Error
		}
		return fmt.Errorf("run %s failed: %s", run.RunID, errStr)
	}
	return nil
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("run_%x", b)
}
