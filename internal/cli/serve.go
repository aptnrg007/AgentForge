package cli

import (
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"agentforge/internal/api"
	"agentforge/internal/mcp"
	"agentforge/internal/store"
)

func newServeCmd() *cobra.Command {
	var addr, dbPath string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the agentforge HTTP daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := store.Open(dbPath)
			if err != nil {
				return err
			}
			defer st.Close()

			logger := slog.Default()
			registry := mcp.NewRegistry(logger)
			defer registry.Close()

			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()

			return api.Serve(ctx, addr, st, registry, logger)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8080", "address to listen on (no auth in v0.1: keep this on localhost)")
	cmd.Flags().StringVar(&dbPath, "db", "agentforge.db", "path to the SQLite run store")
	return cmd
}
