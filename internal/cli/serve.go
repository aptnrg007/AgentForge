package cli

import (
	"context"
	"flag"
	"log/slog"
	"os/signal"
	"syscall"

	"agentforge/internal/api"
	"agentforge/internal/mcp"
	"agentforge/internal/store"
)

func Serve(args []string) error {
	fs := flag.NewFlagSet("agentforge serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on (no auth in v0.1: keep this on localhost)")
	dbPath := fs.String("db", "agentforge.db", "path to the SQLite run store")
	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	logger := slog.Default()
	registry := mcp.NewRegistry(logger)
	defer registry.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	return api.Serve(ctx, *addr, st, registry, logger)
}
