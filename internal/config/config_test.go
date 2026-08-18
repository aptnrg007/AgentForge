package config

import (
	"encoding/json"
	"strings"
	"testing"

	"agentforge/internal/runtime"
)

func TestLoadMinimalExample(t *testing.T) {
	cfg, err := Load("../../examples/minimal.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Name != "minimal" {
		t.Errorf("name = %q, want %q", cfg.Name, "minimal")
	}
	if cfg.Model.Provider != "ollama" || cfg.Model.Name != "qwen2.5-coder:14b" {
		t.Errorf("unexpected model: %+v", cfg.Model)
	}
	if cfg.Instructions == "" {
		t.Error("expected non-empty instructions")
	}
	if cfg.Limits.MaxTurns != 10 {
		t.Errorf("limits.max_turns = %d, want 10", cfg.Limits.MaxTurns)
	}
}

func TestLoadEverythingDemoExample(t *testing.T) {
	cfg, err := Load("../../examples/everything-demo.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.MCP) != 1 || cfg.MCP[0].Name != "everything" {
		t.Fatalf("unexpected mcp servers: %+v", cfg.MCP)
	}
	if len(cfg.Tools) != 2 {
		t.Fatalf("unexpected tools filter: %+v", cfg.Tools)
	}
}

func TestUnknownKeyNamesTheKey(t *testing.T) {
	_, err := Parse([]byte(`
name: bad
modle:
  provider: ollama
  name: foo
`))
	if err == nil {
		t.Fatal("expected an error for the misspelled key")
	}
	if !strings.Contains(err.Error(), "modle") {
		t.Fatalf("expected error to name the misspelled key %q, got: %v", "modle", err)
	}
}

func TestMissingEnvVarFailsAtLoad(t *testing.T) {
	_, err := Parse([]byte(`
name: bad
model:
  provider: ollama
  name: foo
mcp:
  - name: gh
    transport: stdio
    command: ["echo", "hi"]
    env:
      TOKEN: ${AGENTFORGE_TEST_DEFINITELY_UNSET_VAR}
`))
	if err == nil {
		t.Fatal("expected an error for the missing env var")
	}
	if !strings.Contains(err.Error(), "AGENTFORGE_TEST_DEFINITELY_UNSET_VAR") {
		t.Fatalf("expected error to name the missing var, got: %v", err)
	}
}

func TestEnvVarIsInterpolated(t *testing.T) {
	t.Setenv("AGENTFORGE_TEST_TOKEN", "s3cr3t")

	cfg, err := Parse([]byte(`
name: ok
model:
  provider: ollama
  name: foo
mcp:
  - name: gh
    transport: stdio
    command: ["echo", "hi"]
    env:
      TOKEN: ${AGENTFORGE_TEST_TOKEN}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := cfg.MCP[0].Env["TOKEN"]; got != "s3cr3t" {
		t.Fatalf("TOKEN = %q, want %q", got, "s3cr3t")
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "missing model provider",
			yaml: `
name: bad
model:
  name: foo
`,
			wantErr: "provider is required",
		},
		{
			name: "unknown model provider",
			yaml: `
name: bad
model:
  provider: bedrock
  name: foo
`,
			wantErr: `unknown provider "bedrock"`,
		},
		{
			name: "stdio mcp server missing command",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
mcp:
  - name: gh
    transport: stdio
`,
			wantErr: "stdio transport requires command",
		},
		{
			name: "duplicate mcp server name",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
mcp:
  - {name: gh, transport: stdio, command: ["echo"]}
  - {name: gh, transport: stdio, command: ["echo"]}
`,
			wantErr: `duplicate server name "gh"`,
		},
		{
			name: "unknown approvals mode",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
approvals:
  mode: sometimes
`,
			wantErr: `mode: unknown value "sometimes"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestFilterToolsGlob(t *testing.T) {
	tools := []runtime.Tool{
		{Name: "github.search", InputSchema: json.RawMessage(`{}`)},
		{Name: "github.create_issue", InputSchema: json.RawMessage(`{}`)},
		{Name: "search.web_search", InputSchema: json.RawMessage(`{}`)},
	}

	got := FilterTools(tools, []string{"github.*"})
	if len(got) != 2 {
		t.Fatalf("expected 2 tools matching github.*, got %d: %v", len(got), got)
	}
	for _, tool := range got {
		if !strings.HasPrefix(tool.Name, "github.") {
			t.Errorf("unexpected tool passed filter: %s", tool.Name)
		}
	}
}

func TestFilterToolsExactMatch(t *testing.T) {
	tools := []runtime.Tool{
		{Name: "github.search"},
		{Name: "github.create_issue"},
	}
	got := FilterTools(tools, []string{"github.search"})
	if len(got) != 1 || got[0].Name != "github.search" {
		t.Fatalf("expected exactly github.search, got %v", got)
	}
}

func TestFilterToolsEmptyPatternsKeepsAll(t *testing.T) {
	tools := []runtime.Tool{{Name: "a.one"}, {Name: "b.two"}}
	got := FilterTools(tools, nil)
	if len(got) != 2 {
		t.Fatalf("expected all tools kept with no patterns, got %d", len(got))
	}
}
