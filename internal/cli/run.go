// Package cli implements the agentforge command-line interface. Run is a
// placeholder ahead of the cobra commands (serve, run, agents, runs) that
// land in Phase 5; for now it loads one agent from a YAML file and drives a
// single run to completion.
package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/message"
	"agentforge/internal/provider"
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

	var prov provider.Provider
	switch cfg.Model.Provider {
	case "ollama":
		prov = provider.NewOllama(cfg.Model.BaseURL)
	default:
		return fmt.Errorf("model.provider %q not yet supported (only ollama through phase 3)", cfg.Model.Provider)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()

	maxTurns := cfg.Limits.MaxTurns
	if maxTurns == 0 {
		maxTurns = 10
	}

	eng := runtime.NewEngine(st, prov, runtime.Config{
		AgentName:   cfg.Name,
		Model:       cfg.Model.Name,
		System:      cfg.Instructions,
		MaxTurns:    maxTurns,
		MaxTokens:   cfg.Limits.MaxTokens,
		Temperature: cfg.Model.Temperature,
	})

	if len(cfg.MCP) > 0 {
		registry := mcp.NewRegistry(slog.Default())
		defer registry.Close()

		var allTools []runtime.Tool
		for _, srv := range cfg.MCP {
			if srv.Transport != "stdio" {
				return fmt.Errorf("mcp server %q: transport %q not yet supported (only stdio through phase 3)", srv.Name, srv.Transport)
			}
			tools, err := registry.Tools(ctx, srv.Name, mcp.ServerConfig{Command: srv.Command, Env: srv.Env})
			if err != nil {
				return fmt.Errorf("mcp server %q: %w", srv.Name, err)
			}
			allTools = append(allTools, tools...)
		}
		for _, t := range config.FilterTools(allTools, cfg.Tools) {
			eng.RegisterTool(t)
		}
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
