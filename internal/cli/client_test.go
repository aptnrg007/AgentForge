package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDoAPIRequestSetsBearerToken covers the client-side half of `serve
// --auth-token`: a daemon started with a token is unreachable from
// run/runs/agents --server without this, since every request would 401.
func TestDoAPIRequestSetsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := apiGet(context.Background(), srv.URL, "secret-token", nil); err != nil {
		t.Fatalf("apiGet: %v", err)
	}
	if want := "Bearer secret-token"; gotAuth != want {
		t.Errorf("Authorization header = %q, want %q", gotAuth, want)
	}
}

// TestDoAPIRequestNoTokenNoHeader covers the common case (no auth
// configured) to guard against a regression that always sends a header.
func TestDoAPIRequestNoTokenNoHeader(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") != ""
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := apiGet(context.Background(), srv.URL, "", nil); err != nil {
		t.Fatalf("apiGet: %v", err)
	}
	if sawAuth {
		t.Error("Authorization header sent with an empty token")
	}
}
