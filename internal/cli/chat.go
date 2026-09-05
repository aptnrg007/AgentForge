package cli

import (
	"context"
	"log/slog"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

func newChatCmd() *cobra.Command {
	var dbPath string

	cmd := &cobra.Command{
		Use:   "chat <agent.yaml>",
		Short: "Interactive chat with an agent, with approval prompts for gated tool calls",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChat(cmd.Context(), dbPath, args[0])
		},
	}
	cmd.Flags().StringVar(&dbPath, "db", defaultDBPath(), "path to the SQLite run store")
	return cmd
}

func runChat(ctx context.Context, dbPath, cfgPath string) error {
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
	// output: is meant to gate a one-shot run's final answer; forcing
	// every reply in an interactive session to conform to it would make
	// chat unusable, so chat is the one caller that opts out.
	eng.ClearOutputPolicy()

	p := tea.NewProgram(newChatModel(ctx, eng, st, cfg.Name))
	// p.Send is safe to call from any goroutine, including the one
	// OnEvent's callback runs on — the same goroutine tea runs a Cmd
	// (stepCmd) on, blocked inside eng.Step for the duration of a turn.
	// This is what turns eng.Step's synchronous token/tool_call/
	// tool_result callbacks into live updates on the chat transcript
	// instead of one atomic block once Step returns.
	eng.OnEvent(func(ev runtime.Event) { p.Send(ev) })

	_, err = p.Run()
	return err
}
