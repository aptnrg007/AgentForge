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
	cmd.Flags().StringVar(&dbPath, "db", "agentforge.db", "path to the SQLite run store")
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

	_, err = tea.NewProgram(newChatModel(ctx, eng, st, cfg.Name)).Run()
	return err
}
