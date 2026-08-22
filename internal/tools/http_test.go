package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentforge/internal/config"
)

func mustBuildOne(t *testing.T, def config.ToolDefinition) func(ctx context.Context, input json.RawMessage) (string, error) {
	t.Helper()
	cfg := &config.Config{
		Name:            "test",
		Model:           config.ModelConfig{Provider: "ollama", Name: "test-model"},
		ToolDefinitions: []config.ToolDefinition{def},
	}
	tools, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	return tools[0].Execute
}

func objSchema(props ...string) json.RawMessage {
	// Every prop is a required string field. Good enough for these tests.
	b := &strings.Builder{}
	b.WriteString(`{"type":"object","required":[`)
	for i, p := range props {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + p + `"`)
	}
	b.WriteString(`],"properties":{`)
	for i, p := range props {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"` + p + `":{"type":"string"}`)
	}
	b.WriteString(`}}`)
	return json.RawMessage(b.String())
}

func TestHTTPToolQueryValueIsURLEncoded(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	exec := mustBuildOne(t, config.ToolDefinition{
		Name: "search", Description: "test", InputSchema: objSchema("q"),
		HTTP: &config.HTTPToolConfig{
			URL:   srv.URL,
			Query: map[string]string{"q": "{{.q}}"},
		},
	})

	out, err := exec(context.Background(), json.RawMessage(`{"q":"New York & co"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if out != "ok" {
		t.Fatalf("out = %q", out)
	}
	if gotQuery != "q=New+York+%26+co" {
		t.Fatalf("query = %q, want properly encoded", gotQuery)
	}
}

func TestHTTPToolURLPathIsEscaped(t *testing.T) {
	// r.URL.Path is always the *decoded* path (net/http decodes it while
	// routing), so it can't distinguish an escaped "/" from a literal
	// one. r.URL.EscapedPath() preserves what was actually on the wire,
	// which is what proves escaping happened at all.
	var gotEscaped, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEscaped = r.URL.EscapedPath()
		gotPath = r.URL.Path
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	exec := mustBuildOne(t, config.ToolDefinition{
		Name: "get", Description: "test", InputSchema: objSchema("city"),
		HTTP: &config.HTTPToolConfig{URL: srv.URL + "/city/{{.city}}"},
	})

	// A "/" in the input must not be treated as a path separator: with
	// escaping it stays part of the one "city" segment, decoding back to
	// exactly the input.
	if _, err := exec(context.Background(), json.RawMessage(`{"city":"New York/NY"}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(gotEscaped, "%2F") {
		t.Fatalf("escaped path = %q, want the embedded / percent-encoded (%%2F), not a literal path separator", gotEscaped)
	}
	if gotPath != "/city/New York/NY" {
		t.Fatalf("decoded path = %q, want /city/New York/NY (the escaping round-trips back to the exact input)", gotPath)
	}
}

func TestHTTPToolHeaderInterpolation(t *testing.T) {
	t.Setenv("TEST_TOKEN", "sekret")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	// Env interpolation happens at config.Parse time, so build the
	// definition the way a real config would — through YAML — to prove
	// the ${VAR} already landed in Headers before Build ever sees it.
	raw := []byte(`
name: envtest
model: {provider: ollama, name: m}
tool_definitions:
  - name: get
    description: test
    input_schema: {"type":"object"}
    http:
      url: ` + srv.URL + `
      headers:
        Authorization: "Bearer ${TEST_TOKEN}"
`)
	cfg, err := config.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tools, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if _, err := tools[0].Execute(context.Background(), json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if gotAuth != "Bearer sekret" {
		t.Fatalf("Authorization = %q, want %q", gotAuth, "Bearer sekret")
	}
}

func TestHTTPToolNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("not found here"))
	}))
	defer srv.Close()

	exec := mustBuildOne(t, config.ToolDefinition{
		Name: "get", Description: "test", InputSchema: json.RawMessage(`{"type":"object"}`),
		HTTP: &config.HTTPToolConfig{URL: srv.URL},
	})

	_, err := exec(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "not found here") {
		t.Fatalf("error = %v, want it to mention the status and body", err)
	}
}

func TestHTTPToolResponseTruncatedAtCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()

	exec := mustBuildOne(t, config.ToolDefinition{
		Name: "get", Description: "test", InputSchema: json.RawMessage(`{"type":"object"}`),
		HTTP: &config.HTTPToolConfig{URL: srv.URL, MaxResponseBytes: 10},
	})

	out, err := exec(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.HasPrefix(out, strings.Repeat("x", 10)) || !strings.HasSuffix(out, "[truncated]") {
		t.Fatalf("out = %q, want 10 x's then a truncated marker", out)
	}
}

func TestHTTPToolMissingRequiredInputIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("request should never be sent when input is invalid")
	}))
	defer srv.Close()

	exec := mustBuildOne(t, config.ToolDefinition{
		Name: "get", Description: "test", InputSchema: objSchema("q"),
		HTTP: &config.HTTPToolConfig{URL: srv.URL + "/{{.q}}"},
	})

	_, err := exec(context.Background(), json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected an error for missing required input")
	}
	if !strings.Contains(err.Error(), "input_schema") {
		t.Fatalf("error = %v, want it to mention input_schema", err)
	}
}
