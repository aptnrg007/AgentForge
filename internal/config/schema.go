// Package config loads and validates the agentforge YAML schema described
// in PLAN.md section 4: YAML -> JSON (sigs.k8s.io/yaml) -> these structs.
// Unknown keys are load errors (ground rule 5), and ${ENV_VAR} references
// are resolved at load time so a typo'd or missing secret fails immediately
// instead of at tool-call time (ground rule 3).
package config

// Config is the top-level agent definition.
type Config struct {
	Name         string          `json:"name"`
	Model        ModelConfig     `json:"model"`
	Instructions string          `json:"instructions,omitempty"`
	MCP          []MCPServer     `json:"mcp,omitempty"`
	Tools        []string        `json:"tools,omitempty"`
	Approvals    ApprovalsConfig `json:"approvals,omitempty"`
	Limits       LimitsConfig    `json:"limits,omitempty"`
	Session      SessionConfig   `json:"session,omitempty"`
	Output       OutputConfig    `json:"output,omitempty"`

	// SourceDir is the directory Load read this config's file from, used
	// to resolve output.schema (and any future relative path) against
	// the config's own location rather than the process's cwd. json:"-"
	// keeps it invisible to yaml.UnmarshalStrict's unknown-key check, so
	// a stray "sourceDir:" in YAML still errors like any other typo.
	// Empty when the config came from Parse instead of Load (e.g. every
	// reconstruction from YAML stored in SQLite) — there is no source
	// file in that case.
	SourceDir string `json:"-"`
}

type ModelConfig struct {
	Provider    string  `json:"provider"`
	Name        string  `json:"name"`
	Temperature float64 `json:"temperature,omitempty"`
	BaseURL     string  `json:"base_url,omitempty"`
	// APIKey is typically an ${ENV_VAR} reference, resolved like any other
	// config string (env.go) — so a missing key fails at load time with a
	// clear message, not on the first request.
	APIKey string `json:"api_key,omitempty"`
}

// MCPServer describes one MCP server to connect to. Only Transport: stdio
// is wired up as of Phase 3 — http is parsed and validated but rejected at
// use until a later phase implements that transport.
type MCPServer struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Command   []string          `json:"command,omitempty"`
	URL       string            `json:"url,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

// ApprovalsConfig is parsed and validated starting in Phase 3, but not
// enforced by the runtime until Phase 6.
type ApprovalsConfig struct {
	Mode        string   `json:"mode,omitempty"`
	Require     []string `json:"require,omitempty"`
	AutoApprove []string `json:"auto_approve,omitempty"`
	Timeout     string   `json:"timeout,omitempty"`
	OnTimeout   string   `json:"on_timeout,omitempty"`
}

type LimitsConfig struct {
	MaxTurns  int    `json:"max_turns,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	Timeout   string `json:"timeout,omitempty"`
}

// SessionConfig is parsed and validated starting in Phase 3, but session
// persistence itself isn't implemented until a later phase.
type SessionConfig struct {
	Type string `json:"type,omitempty"`
}

// OutputConfig turns on schema-validated structured output for a run's
// final answer (not tool calls). Only applies to one-shot runs (`run`,
// the HTTP API) — internal/cli/chat.go explicitly opts a chat session out
// via Engine.ClearOutputPolicy, since forcing every conversational reply
// to conform to a schema would make interactive chat unusable.
type OutputConfig struct {
	// Schema is a path to a JSON Schema file (draft-07 or draft
	// 2020-12), resolved relative to the config file's own directory
	// when relative and the config was loaded from a file (see
	// Config.SourceDir) — otherwise relative to the process's cwd.
	Schema string `json:"schema,omitempty"`
	// OnInvalid is "retry" (default) or "fail".
	OnInvalid string `json:"on_invalid,omitempty"`
	// MaxRetries caps consecutive schema-violation turns; 0 means "use
	// the engine's default" (2, matching the tool-call repair ceiling),
	// not "no retries" — on_invalid: fail is how you get zero retries.
	MaxRetries int `json:"max_retries,omitempty"`
}
