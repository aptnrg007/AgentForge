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
	return s.logRequests(mux)
}

// logRequests wraps h with one Info line per request — method, path,
// status, and how long it took — the request-level counterpart to
// internal/runtime's per-run state-transition logging. Every handler
// already has ctx for a request-scoped logger if one's ever needed; this
// stays a flat wrapper since nothing here currently correlates multiple
// log lines to one request the way run_id does for a run.
func (s *Server) logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(sw, r)
		s.logger.Info("api: request", "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

// statusWriter captures the status code a handler wrote, since
// http.ResponseWriter itself doesn't expose it after the fact — WriteHeader
// is never called at all for a handler that only calls Write (net/http
// implicitly sends 200 in that case), so status starts at 200 accordingly.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(status int) {
	sw.status = status
	sw.ResponseWriter.WriteHeader(status)
}

// Flush satisfies http.Flusher by delegating to the wrapped
// ResponseWriter — required so logRequests' wrapping doesn't break
// newSSEWriter's `w.(http.Flusher)` type assertion for handleStreamAgent,
// which would otherwise see a statusWriter with no Flush method and
// reject every streaming request outright.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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
