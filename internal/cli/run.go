// Package cli implements the agentforge command-line interface. Run is a
// Phase 1 placeholder that hardcodes agent config and drives one run to
// completion; it's replaced by cobra commands (serve, run, agents, runs)
// in Phase 5 once YAML config exists.
package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"agentforge/internal/mcp"
	"agentforge/internal/message"
	"agentforge/internal/provider"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

func Run(args []string) error {
	fs := flag.NewFlagSet("agentforge", flag.ContinueOnError)
	dbPath := fs.String("db", "agentforge.db", "path to the SQLite run store")
	model := fs.String("model", "qwen2.5-coder:14b", "Ollama model name")
	baseURL := fs.String("base-url", "", "Ollama base URL (default http://localhost:11434)")
	maxTurns := fs.Int("max-turns", 10, "maximum model turns before the run fails")
	withMCP := fs.Bool("mcp", false, "also register tools from the @modelcontextprotocol/server-everything reference MCP server")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf(`usage: agentforge [flags] "<message>"`)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()

	eng := runtime.NewEngine(st, provider.NewOllama(*baseURL), runtime.Config{
		AgentName:   "demo",
		Model:       *model,
		System:      "You are a helpful assistant. Use the echo tool if the user asks you to echo something.",
		MaxTurns:    *maxTurns,
		MaxTokens:   1024,
		Temperature: 0.2,
	})
	eng.RegisterTool(runtime.NewEchoTool())

	if *withMCP {
		registry := mcp.NewRegistry(slog.Default())
		defer registry.Close()
		tools, err := registry.Tools(ctx, "everything", mcp.ServerConfig{
			Command: []string{"npx", "-y", "@modelcontextprotocol/server-everything"},
		})
		if err != nil {
			return fmt.Errorf("connect to MCP server: %w", err)
		}
		for _, t := range tools {
			eng.RegisterTool(t)
		}
	}

	runID := fmt.Sprintf("run_%d", os.Getpid())
	if err := eng.NewRun(ctx, runID, fs.Arg(0)); err != nil {
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
