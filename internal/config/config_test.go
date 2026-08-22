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

func TestLoadStructuredOutputExample(t *testing.T) {
	cfg, err := Load("../../examples/structured-output.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Output.Schema != "./schemas/story-spec.json" {
		t.Fatalf("output.schema = %q, want %q", cfg.Output.Schema, "./schemas/story-spec.json")
	}
	if cfg.Output.OnInvalid != "retry" {
		t.Fatalf("output.on_invalid = %q, want %q", cfg.Output.OnInvalid, "retry")
	}
	if cfg.Output.MaxRetries != 2 {
		t.Fatalf("output.max_retries = %d, want 2", cfg.Output.MaxRetries)
	}
}

func TestLoadSetsSourceDirButParseDoesNot(t *testing.T) {
	cfg, err := Load("../../examples/minimal.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SourceDir != "../../examples" {
		t.Fatalf("SourceDir = %q, want %q", cfg.SourceDir, "../../examples")
	}

	parsed, err := Parse([]byte(`
name: ok
model: {provider: ollama, name: foo}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed.SourceDir != "" {
		t.Fatalf("expected Parse to leave SourceDir empty, got %q", parsed.SourceDir)
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

func TestModelAPIKeyIsInterpolated(t *testing.T) {
	t.Setenv("AGENTFORGE_TEST_ANTHROPIC_KEY", "sk-ant-test")

	cfg, err := Parse([]byte(`
name: ok
model:
  provider: anthropic
  name: claude-sonnet-4-6
  api_key: ${AGENTFORGE_TEST_ANTHROPIC_KEY}
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Model.APIKey != "sk-ant-test" {
		t.Fatalf("Model.APIKey = %q, want %q", cfg.Model.APIKey, "sk-ant-test")
	}
}

func TestMissingModelAPIKeyEnvVarFailsAtLoad(t *testing.T) {
	_, err := Parse([]byte(`
name: bad
model:
  provider: anthropic
  name: claude-sonnet-4-6
  api_key: ${AGENTFORGE_TEST_DEFINITELY_UNSET_ANTHROPIC_KEY}
`))
	if err == nil {
		t.Fatal("expected an error for the missing api_key env var")
	}
	if !strings.Contains(err.Error(), "AGENTFORGE_TEST_DEFINITELY_UNSET_ANTHROPIC_KEY") {
		t.Fatalf("expected error to name the missing var, got: %v", err)
	}
}

// TestOpenAIWithBaseURLDoesNotRequireAPIKey confirms the exemption for a
// local/self-hosted OpenAI-compatible server (vLLM, llama.cpp, LM
// Studio), which typically ignores auth entirely — only real OpenAI
// (base_url empty) requires a key.
func TestOpenAIWithBaseURLDoesNotRequireAPIKey(t *testing.T) {
	_, err := Parse([]byte(`
name: ok
model:
  provider: openai
  name: local-model
  base_url: http://localhost:8000/v1
`))
	if err != nil {
		t.Fatalf("expected no error for openai with base_url and no api_key: %v", err)
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
		{
			name: "anthropic without api_key",
			yaml: `
name: bad
model: {provider: anthropic, name: claude-sonnet-4-6}
`,
			wantErr: `api_key is required for provider "anthropic"`,
		},
		{
			name: "openai without api_key or base_url",
			yaml: `
name: bad
model: {provider: openai, name: gpt-5}
`,
			wantErr: `api_key is required for provider "openai"`,
		},
		{
			name: "unknown output on_invalid",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
output:
  schema: ./schema.json
  on_invalid: sometimes
`,
			wantErr: `on_invalid: unknown value "sometimes"`,
		},
		{
			name: "negative output max_retries",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
output:
  schema: ./schema.json
  max_retries: -1
`,
			wantErr: "max_retries: must be >= 0",
		},
		{
			name: "output on_invalid without schema",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
output:
  on_invalid: fail
`,
			wantErr: "on_invalid/max_retries set without schema",
		},
		{
			name: "unknown tool_policy on_timeout",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_policy:
  timeout: 30s
  on_timeout: sometimes
`,
			wantErr: `on_timeout: unknown value "sometimes"`,
		},
		{
			name: "unparseable tool_policy timeout",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_policy:
  timeout: soon
`,
			wantErr: "timeout:",
		},
		{
			name: "zero tool_policy timeout",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_policy:
  timeout: 0s
`,
			wantErr: `timeout: must be > 0, got "0s"`,
		},
		{
			name: "negative tool_policy timeout",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_policy:
  timeout: -5s
`,
			wantErr: `timeout: must be > 0, got "-5s"`,
		},
		{
			name: "tool_policy on_timeout without timeout",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_policy:
  on_timeout: fail
`,
			wantErr: "on_timeout/overrides set without timeout",
		},
		{
			name: "tool_policy override without tools",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_policy:
  timeout: 30s
  overrides:
    - timeout: 90s
`,
			wantErr: "overrides[0]: tools is required",
		},
		{
			name: "tool_policy override with unparseable timeout",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_policy:
  timeout: 30s
  overrides:
    - tools: ["github.*"]
      timeout: soon
`,
			wantErr: "overrides[0].timeout:",
		},
		{
			name: "tool_definitions missing name",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - description: does a thing
    input_schema: {"type": "object"}
    http: {url: "https://example.com"}
`,
			wantErr: "tool_definitions[0]: name is required",
		},
		{
			name: "tool_definitions name with glob metacharacter",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - name: "repo.*"
    description: does a thing
    input_schema: {"type": "object"}
    http: {url: "https://example.com"}
`,
			wantErr: "must match",
		},
		{
			name: "tool_definitions missing description",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - name: repo.grep
    input_schema: {"type": "object"}
    http: {url: "https://example.com"}
`,
			wantErr: "description is required",
		},
		{
			name: "tool_definitions missing input_schema",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - name: repo.grep
    description: does a thing
    http: {url: "https://example.com"}
`,
			wantErr: "input_schema is required",
		},
		{
			name: "tool_definitions uncompilable input_schema",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - name: repo.grep
    description: does a thing
    input_schema: {"type": ["object", 5]}
    http: {url: "https://example.com"}
`,
			wantErr: "input_schema:",
		},
		{
			name: "tool_definitions neither http nor command",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - name: repo.grep
    description: does a thing
    input_schema: {"type": "object"}
`,
			wantErr: "exactly one of http/command is required",
		},
		{
			name: "tool_definitions both http and command",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - name: repo.grep
    description: does a thing
    input_schema: {"type": "object"}
    http: {url: "https://example.com"}
    command: {argv: ["rg"]}
`,
			wantErr: "only one of http/command may be set",
		},
		{
			name: "tool_definitions duplicate name",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - name: repo.grep
    description: does a thing
    input_schema: {"type": "object"}
    http: {url: "https://example.com"}
  - name: repo.grep
    description: does another thing
    input_schema: {"type": "object"}
    http: {url: "https://example.com"}
`,
			wantErr: `duplicate name "repo.grep"`,
		},
		{
			name: "tool_definitions name collides with mcp namespace",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
mcp:
  - {name: repo, transport: stdio, command: ["echo"]}
tool_definitions:
  - name: repo.grep
    description: does a thing
    input_schema: {"type": "object"}
    http: {url: "https://example.com"}
`,
			wantErr: `collides with mcp server "repo"'s namespace`,
		},
		{
			name: "tool_definitions excluded by tools filter",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - name: repo.grep
    description: does a thing
    input_schema: {"type": "object"}
    http: {url: "https://example.com"}
tools:
  - "other.*"
`,
			wantErr: "excluded by the tools: filter",
		},
		{
			name: "tool_definitions unparseable template",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - name: repo.grep
    description: does a thing
    input_schema: {"type": "object"}
    http: {url: "https://example.com/{{.pattern}"}
`,
			wantErr: "url:",
		},
		{
			name: "tool_definitions unknown http method",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - name: repo.grep
    description: does a thing
    input_schema: {"type": "object"}
    http: {url: "https://example.com", method: FROBNICATE}
`,
			wantErr: `method: unknown value "FROBNICATE"`,
		},
		{
			name: "tool_definitions templated argv[0]",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - name: repo.grep
    description: does a thing
    input_schema: {"type": "object"}
    command: {argv: ["{{.binary}}"]}
`,
			wantErr: "argv[0]: must be literal",
		},
		{
			name: "tool_definitions templated url host",
			yaml: `
name: bad
model: {provider: ollama, name: foo}
tool_definitions:
  - name: repo.grep
    description: does a thing
    input_schema: {"type": "object"}
    http: {url: "https://{{.host}}/path"}
`,
			wantErr: "scheme and host must be literal",
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

func TestLoadWeatherExample(t *testing.T) {
	cfg, err := Load("../../examples/weather.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.MCP) != 1 || cfg.MCP[0].Name != "web" {
		t.Fatalf("unexpected mcp servers: %+v", cfg.MCP)
	}
	if cfg.ToolPolicy.Timeout != "20s" {
		t.Errorf("tool_policy.timeout = %q, want %q", cfg.ToolPolicy.Timeout, "20s")
	}
	if cfg.ToolPolicy.OnTimeout != "error" {
		t.Errorf("tool_policy.on_timeout = %q, want %q", cfg.ToolPolicy.OnTimeout, "error")
	}
}

func TestLoadWeatherHTTPExample(t *testing.T) {
	cfg, err := Load("../../examples/weather-http.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.MCP) != 0 {
		t.Fatalf("expected no mcp servers, got %+v", cfg.MCP)
	}
	if len(cfg.ToolDefinitions) != 2 {
		t.Fatalf("expected 2 tool_definitions, got %d", len(cfg.ToolDefinitions))
	}
	geo := cfg.ToolDefinitions[0]
	if geo.Name != "geo.search" || geo.HTTP == nil || geo.Command != nil {
		t.Fatalf("unexpected first tool_definitions entry: %+v", geo)
	}
	if geo.HTTP.URL != "https://geocoding-api.open-meteo.com/v1/search" {
		t.Errorf("geo.search url = %q", geo.HTTP.URL)
	}
}

func TestLoadRepoAssistantExample(t *testing.T) {
	cfg, err := Load("../../examples/repo-assistant.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.ToolDefinitions) != 2 {
		t.Fatalf("expected 2 tool_definitions, got %d", len(cfg.ToolDefinitions))
	}
	grep := cfg.ToolDefinitions[0]
	if grep.Name != "repo.grep" || grep.Command == nil || grep.HTTP != nil {
		t.Fatalf("unexpected first tool_definitions entry: %+v", grep)
	}
	if len(cfg.Approvals.AutoApprove) != 1 || cfg.Approvals.AutoApprove[0] != "repo.log" {
		t.Errorf("approvals.auto_approve = %v, want [\"repo.log\"]", cfg.Approvals.AutoApprove)
	}
}

func TestLoadArticleDigestExample(t *testing.T) {
	cfg, err := Load("../../examples/article-digest.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.MCP) == 0 {
		t.Fatalf("expected at least one mcp server, got none")
	}
	if cfg.Output.Schema != "./schemas/digest.json" {
		t.Fatalf("output.schema = %q, want %q", cfg.Output.Schema, "./schemas/digest.json")
	}
}

func TestLoadAnthropicExample(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	cfg, err := Load("../../examples/anthropic.yaml")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Model.Provider != "anthropic" {
		t.Errorf("model.provider = %q, want %q", cfg.Model.Provider, "anthropic")
	}
	if cfg.Model.APIKey != "sk-ant-test" {
		t.Errorf("model.api_key = %q, want interpolated %q", cfg.Model.APIKey, "sk-ant-test")
	}
}

func TestLoadToolPolicy(t *testing.T) {
	cfg, err := Parse([]byte(`
name: ok
model: {provider: ollama, name: foo}
tool_policy:
  timeout: 30s
  on_timeout: fail
  overrides:
    - tools: ["github.*"]
      timeout: 90s
    - tools: ["render.video", "build.*"]
      timeout: 10m
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.ToolPolicy.Timeout != "30s" {
		t.Errorf("tool_policy.timeout = %q, want %q", cfg.ToolPolicy.Timeout, "30s")
	}
	if cfg.ToolPolicy.OnTimeout != "fail" {
		t.Errorf("tool_policy.on_timeout = %q, want %q", cfg.ToolPolicy.OnTimeout, "fail")
	}
	if len(cfg.ToolPolicy.Overrides) != 2 {
		t.Fatalf("expected 2 overrides, got %d", len(cfg.ToolPolicy.Overrides))
	}
	if got := cfg.ToolPolicy.Overrides[0]; len(got.Tools) != 1 || got.Tools[0] != "github.*" || got.Timeout != "90s" {
		t.Errorf("overrides[0] = %+v, want tools=[github.*] timeout=90s", got)
	}
	if got := cfg.ToolPolicy.Overrides[1]; len(got.Tools) != 2 || got.Tools[1] != "build.*" || got.Timeout != "10m" {
		t.Errorf("overrides[1] = %+v, want tools=[render.video build.*] timeout=10m", got)
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
