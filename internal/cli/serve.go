package cli

import (
	"log/slog"

	"github.com/spf13/cobra"

	"agentforge/internal/api"
	"agentforge/internal/mcp"
	"agentforge/internal/store"
)

func newServeCmd() *cobra.Command {
	var addr, dbPath, authToken string

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the agentforge HTTP daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ensureDBDir(dbPath); err != nil {
				return err
			}
			st, err := store.Open(dbPath)
			if err != nil {
				return err
			}
			defer st.Close()

			logger := slog.Default()
			registry := mcp.NewRegistry(logger)
			defer registry.Close()

			// cmd.Context() is already SIGINT/SIGTERM-cancellable — see
			// main.go and root.go's Execute — so no need for serve to
			// install its own signal handling anymore.
			return api.Serve(cmd.Context(), addr, st, registry, logger, authToken)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", "127.0.0.1:8080", "address to listen on (safe to leave unauthenticated only while this stays loopback-only)")
	cmd.Flags().StringVar(&dbPath, "db", defaultDBPath(), "path to the SQLite run store")
	cmd.Flags().StringVar(&authToken, "auth-token", "", `require "Authorization: Bearer <token>" on every request but /healthz; empty (default) means no auth`)
	return cmd
}
