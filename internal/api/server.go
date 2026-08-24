// Package api implements the agentforge HTTP daemon described in
// docs/DESIGN.md section 9. No auth in v0.1: bind to 127.0.0.1 and treat
// that as the security boundary.
package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"agentforge/internal/agent"
	"agentforge/internal/mcp"
	"agentforge/internal/store"
)

type Server struct {
	store           *store.Store
	registry        *mcp.Registry
	logger          *slog.Logger
	providerFactory agent.ProviderFactory
}

func NewServer(st *store.Store, registry *mcp.Registry, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		store:           st,
		registry:        registry,
		logger:          logger,
		providerFactory: agent.DefaultProviderFactory,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /v1/agents", s.handleCreateAgent)
	mux.HandleFunc("GET /v1/agents", s.handleListAgents)
	mux.HandleFunc("GET /v1/agents/{name}", s.handleGetAgent)
	mux.HandleFunc("DELETE /v1/agents/{name}", s.handleDeleteAgent)
	mux.HandleFunc("GET /v1/agents/{name}/tools", s.handleAgentTools)
	mux.HandleFunc("POST /v1/agents/{name}/run", s.handleRunAgent)
	mux.HandleFunc("POST /v1/agents/{name}/stream", s.handleStreamAgent)
	mux.HandleFunc("GET /v1/runs", s.handleListRuns)
	mux.HandleFunc("GET /v1/runs/{id}", s.handleGetRun)
	mux.HandleFunc("POST /v1/runs/{id}/approve", s.handleApprove)
	mux.HandleFunc("POST /v1/runs/{id}/resume", s.handleResume)
	mux.HandleFunc("POST /v1/runs/{id}/cancel", s.handleCancel)
	return mux
}

// Serve runs the HTTP daemon until ctx is cancelled, then shuts it down
// gracefully (in-flight requests get up to 10s to finish).
func Serve(ctx context.Context, addr string, st *store.Store, registry *mcp.Registry, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	srv := NewServer(st, registry, logger)
	httpServer := &http.Server{Addr: addr, Handler: srv.Handler()}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api: listening", "addr", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		logger.Info("api: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return <-errCh
	case err := <-errCh:
		return err
	}
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("run_%x", b)
}
