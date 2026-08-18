// Package cli implements the agentforge command-line interface. Run and
// Serve are placeholders ahead of the cobra commands (serve, run, agents,
// runs) that land in Phase 5.
package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/message"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

func Run(args []string) error {
	fs := flag.NewFlagSet("agentforge", flag.ContinueOnError)
	dbPath := fs.String("db", "agentforge.db", "path to the SQLite run store")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf(`usage: agentforge [flags] <agent.yaml> "<message>"`)
	}
	cfgPath, userMessage := fs.Arg(0), fs.Arg(1)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	registry := mcp.NewRegistry(slog.Default())
	defer registry.Close()

	ctx := context.Background()

	eng, err := agent.Build(ctx, st, registry, cfg, agent.DefaultProviderFactory)
	if err != nil {
		return err
	}

	runID := fmt.Sprintf("run_%d", os.Getpid())
	if err := eng.NewRun(ctx, runID, userMessage); err != nil {
		return err
	}

	for {
		state, err := eng.Step(ctx, runID)
		if err != nil {
			return err
		}
		fmt.Printf("[%s] state=%s\n", runID, state)
		if state == runtime.StateCompleted || state == runtime.StateFailed || state == runtime.StateAwaitingApproval {
			if err := printTrace(ctx, st, runID); err != nil {
				return err
			}
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

func printTrace(ctx context.Context, st *store.Store, runID string) error {
	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		for _, b := range m.Content {
			switch b.Type {
			case message.BlockText:
				fmt.Printf("%s: %s\n", m.Role, b.Text)
			case message.BlockToolUse:
				fmt.Printf("%s: tool_use %s(%s)\n", m.Role, b.Name, string(b.Input))
			case message.BlockToolResult:
				fmt.Printf("%s: tool_result[%s] %s\n", m.Role, b.ToolUseID, b.Content)
			}
		}
	}
	return nil
}
