package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"agentforge/internal/mcp"
	"agentforge/internal/store"
)

// TestHandlerLogsEveryRequest confirms logRequests actually wraps the
// mux — internal/runtime logs state transitions, but before this the API
// had no log line for the request path at all (docs/DESIGN.md section 13,
// "make the run loop audible").
func TestHandlerLogsEveryRequest(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	registry := mcp.NewRegistry(discardLogger())
	defer registry.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	srv := &Server{store: st, registry: registry, logger: logger, providerFactory: fakeProviderFactory()}

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET healthz: %v", err)
	}
	resp.Body.Close()

	out := buf.String()
	if !strings.Contains(out, "api: request") {
		t.Fatalf("log output = %q, want a line containing \"api: request\"", out)
	}
	if !strings.Contains(out, "path=/healthz") {
		t.Fatalf("log output = %q, want it to mention path=/healthz", out)
	}
	if !strings.Contains(out, "status=200") {
		t.Fatalf("log output = %q, want it to mention status=200", out)
	}
}
