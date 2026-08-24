package api

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"agentforge/internal/mcp"
	"agentforge/internal/store"
)

func newAuthTestServer(t *testing.T, authToken string) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	registry := mcp.NewRegistry(discardLogger())
	t.Cleanup(func() { registry.Close() })

	srv := &Server{store: st, registry: registry, logger: discardLogger(), providerFactory: fakeProviderFactory(), authToken: authToken}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestNoAuthTokenMeansEveryRequestSucceeds(t *testing.T) {
	ts := newAuthTestServer(t, "")

	resp, err := http.Get(ts.URL + "/v1/agents")
	if err != nil {
		t.Fatalf("GET /v1/agents: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no auth token configured)", resp.StatusCode)
	}
}

func TestAuthTokenRejectsRequestWithNoHeader(t *testing.T) {
	ts := newAuthTestServer(t, "s3cr3t")

	resp, err := http.Get(ts.URL + "/v1/agents")
	if err != nil {
		t.Fatalf("GET /v1/agents: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthTokenRejectsWrongToken(t *testing.T) {
	ts := newAuthTestServer(t, "s3cr3t")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/agents: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuthTokenAcceptsCorrectToken(t *testing.T) {
	ts := newAuthTestServer(t, "s3cr3t")

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer s3cr3t")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/agents: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestAuthTokenExemptsHealthz(t *testing.T) {
	ts := newAuthTestServer(t, "s3cr3t")

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (/healthz must not require auth)", resp.StatusCode)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8080", true},
		{"localhost:8080", true},
		{"[::1]:8080", true},
		{"0.0.0.0:8080", false},
		{":8080", false},
		{"192.168.1.5:8080", false},
	}
	for _, c := range cases {
		if got := isLoopbackAddr(c.addr); got != c.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", c.addr, got, c.want)
		}
	}
}
