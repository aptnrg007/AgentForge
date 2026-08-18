package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

func newRunCmd() *cobra.Command {
	var (
		dbPath string
		msg    string
		server string
	)

	cmd := &cobra.Command{
		Use:   "run <agent.yaml>",
		Short: "Run an agent once and print the result",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if msg == "" {
				return fmt.Errorf("-m/--message is required")
			}
			if server != "" {
				return runRemote(cmd.Context(), server, args[0], msg)
			}
			return runLocal(cmd.Context(), dbPath, args[0], msg)
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "agentforge.db", "path to the SQLite run store (embedded mode only)")
	cmd.Flags().StringVarP(&msg, "message", "m", "", "message to send the agent")
	cmd.Flags().StringVar(&server, "server", "", "daemon URL to run against instead of an embedded engine, e.g. http://localhost:8080")
	return cmd
}

// runLocal drives the agent with an embedded engine: no daemon involved.
// It still upserts the agent into the local store so `agentforge agents
// list/get` (without --server) can see agents that were run this way.
func runLocal(ctx context.Context, dbPath, cfgPath, msg string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	rawYAML, err := os.ReadFile(cfgPath)
	if err != nil {
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

	for {
		state, err := eng.Step(ctx, runID)
		if err != nil {
			return err
		}
		fmt.Printf("[%s] state=%s\n", runID, state)
		if state == runtime.StateCompleted || state == runtime.StateFailed || state == runtime.StateAwaitingApproval {
			msgs, err := st.ListMessages(ctx, runID)
			if err != nil {
				return err
			}
			printMessages(msgs)
			if state == runtime.StateFailed {
				run, err := st.GetRun(ctx, runID)
				if err != nil {
					return err
				}
				if run.Error != nil {
					fmt.Fprintln(os.Stderr, "run failed:", *run.Error)
				}
			}
			return nil
		}
	}
}

// runRemote registers the agent with a running daemon and runs it there.
func runRemote(ctx context.Context, server, cfgPath, msg string) error {
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
	var run remoteRun
	if err := apiPost(ctx, server+"/v1/agents/"+ag.Name+"/run", "application/json", reqBody, &run); err != nil {
		return fmt.Errorf("run agent: %w", err)
	}

	fmt.Printf("[%s] state=%s\n", run.RunID, run.State)
	printMessages(run.Messages)
	if run.State == "failed" && run.Error != nil {
		fmt.Fprintln(os.Stderr, "run failed:", *run.Error)
	}
	return nil
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("run_%x", b)
}
