// Package api implements the agentforge HTTP daemon described in
// docs/DESIGN.md section 9. Auth is opt-in (--auth-token / the authToken
// parameter below): binding to 127.0.0.1 with no token set is still the
// default and still a reasonable security boundary on its own, but a
// token is required before the daemon can safely listen on anything else.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
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
	// authToken, when non-empty, is required as "Authorization: Bearer
	// <authToken>" on every request except /healthz. Empty (the default)
	// means no auth at all, unchanged from before this existed.
	authToken string
}

// NewServer builds a Server. authToken is optional ("" disables auth
// entirely, matching agentforge's pre-auth behavior) — see Server.authToken.
func NewServer(st *store.Store, registry *mcp.Registry, logger *slog.Logger, authToken string) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		store:           st,
		registry:        registry,
		logger:          logger,
		providerFactory: agent.DefaultProviderFactory,
		authToken:       authToken,
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
	return s.logRequests(s.requireAuth(mux))
}

// requireAuth wraps h with a bearer-token check when s.authToken is set
// — /healthz is exempt (a liveness probe has no business needing a
// secret, and can't always attach headers) but every other route
// requires "Authorization: Bearer <authToken>" or gets a 401. A nil-op
// wrapper when s.authToken == "", matching agentforge's pre-auth
// behavior exactly.
func (s *Server) requireAuth(h http.Handler) http.Handler {
	if s.authToken == "" {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			h.ServeHTTP(w, r)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		// constant-time compare: a naive == leaks how many leading bytes
		// of a guessed token matched via response timing.
		if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) != 1 {
			writeError(w, http.StatusUnauthorized, fmt.Errorf(`missing or invalid bearer token`))
			return
		}
		h.ServeHTTP(w, r)
	})
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
// gracefully (in-flight requests get up to 10s to finish). authToken, if
// non-empty, is required on every request but /healthz — see Server.authToken.
func Serve(ctx context.Context, addr string, st *store.Store, registry *mcp.Registry, logger *slog.Logger, authToken string) error {
	if logger == nil {
		logger = slog.Default()
	}
	srv := NewServer(st, registry, logger, authToken)
	httpServer := &http.Server{Addr: addr, Handler: srv.Handler()}

	if authToken == "" && !isLoopbackAddr(addr) {
		logger.Warn("api: no --auth-token set and addr is not loopback-only; every request is unauthenticated", "addr", addr)
	}

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

// isLoopbackAddr reports whether addr (a "host:port" listen address, as
// passed to --addr) only binds loopback — used solely to decide whether
// Serve's no-auth-token warning fires. An empty or unparseable host is
// treated as non-loopback (binds every interface, or is malformed enough
// that ListenAndServe will reject it momentarily anyway): the warning
// erring toward firing is the safer default here.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	return net.ParseIP(host).IsLoopback()
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("run_%x", b)
}
