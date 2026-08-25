// Package config loads and validates the agentforge YAML schema described
// in docs/DESIGN.md section 4: YAML -> JSON (sigs.k8s.io/yaml) -> these
// structs. Unknown keys are load errors (ground rule 5), and ${ENV_VAR}
// references are resolved at load time so a typo'd or missing secret fails
// immediately instead of at tool-call time (ground rule 3).
package config

import "encoding/json"

// Config is the top-level agent definition.
type Config struct {
	Name            string           `json:"name"`
	Model           ModelConfig      `json:"model"`
	Instructions    string           `json:"instructions,omitempty"`
	MCP             []MCPServer      `json:"mcp,omitempty"`
	ToolDefinitions []ToolDefinition `json:"tool_definitions,omitempty"`
	Tools           []string         `json:"tools,omitempty"`
	Approvals       ApprovalsConfig  `json:"approvals,omitempty"`
	Limits          LimitsConfig     `json:"limits,omitempty"`
	Output          OutputConfig     `json:"output,omitempty"`
	ToolPolicy      ToolPolicyConfig `json:"tool_policy,omitempty"`
	Retry           RetryConfig      `json:"retry,omitempty"`

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
	// NumCtx sets Ollama's context-window size (its "options.num_ctx" chat
	// request field). Ollama-only — no other provider has an equivalent
	// request-time knob, so validate.go rejects this being set alongside
	// any other provider rather than silently ignoring it (ground rule 3:
	// fail at load time, not three retries in).
	NumCtx int `json:"num_ctx,omitempty"`
	// Think controls Ollama's top-level "think" chat request field for
	// hybrid-reasoning models (e.g. qwen3): true/false forces the
	// thinking channel on or off, nil leaves the model/Ollama default in
	// place. Measured live against qwen3:8b with tools registered: the
	// model sometimes narrates its entire plan into the thinking channel
	// and then emits neither a tool call nor any content — Ollama reports
	// this as a clean done_reason "stop", not a truncation, so raising
	// max_tokens does not help. think:false removes the failure mode
	// entirely by removing the channel it happens in. Ollama-only, same
	// validation rule as NumCtx.
	Think *bool `json:"think,omitempty"`
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

// ToolDefinition declares one in-process tool: a name, a description and
// input_schema the model sees exactly like an MCP tool's, backed by
// either an HTTP request or an exec'd command instead of an MCP server.
// Exactly one of HTTP/Command must be set — see ToolDefinition.validate.
// This is the narrow, one-shot alternative to writing an MCP server; see
// the "Defining your own tools" section of README.md for when to reach
// for which.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	// ReadOnly mirrors MCP's readOnlyHint annotation, and like that
	// annotation is only consulted under approvals.mode: annotated.
	// Defaults false for both backends — an HTTP GET is not assumed
	// read-only just because it's a GET.
	ReadOnly bool `json:"read_only,omitempty"`

	HTTP    *HTTPToolConfig    `json:"http,omitempty"`
	Command *CommandToolConfig `json:"command,omitempty"`
}

// HTTPToolConfig backs a ToolDefinition with a single HTTP request. Every
// string field (except Method) is a text/template against the tool
// call's input; see internal/tools/template.go.
type HTTPToolConfig struct {
	Method  string            `json:"method,omitempty"` // default GET
	URL     string            `json:"url"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
	// MaxResponseBytes caps the response body read from the server; 0
	// means the internal/tools default (64 KiB).
	MaxResponseBytes int `json:"max_response_bytes,omitempty"`
}

// CommandToolConfig backs a ToolDefinition with an exec'd process — no
// shell involved, ever: Argv is passed to exec.Command as discrete
// arguments, so a rendered value containing shell metacharacters is
// inert. Every string field is a text/template against the tool call's
// input. A command-backed tool defaults to approval-gated regardless of
// approvals.mode; see runtime.Tool.RequiresApproval.
type CommandToolConfig struct {
	Argv    []string          `json:"argv"`
	Workdir string            `json:"workdir,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Stdin   string            `json:"stdin,omitempty"`
	// MaxOutputBytes caps the captured stdout; 0 means the
	// internal/tools default (64 KiB).
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
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

// ToolPolicyConfig bounds how long a single tool call may run. Absent
// entirely (the zero value), tools are unbounded — the pre-existing
// behavior, unchanged unless a config opts in.
type ToolPolicyConfig struct {
	// Timeout is the default applied to every tool call unless an
	// override matches. Empty means unbounded.
	Timeout string `json:"timeout,omitempty"`
	// OnTimeout is "error" (default, empty) — the timeout becomes a
	// tool-result error fed back to the model, same as a denied call —
	// or "fail", which ends the run.
	OnTimeout string `json:"on_timeout,omitempty"`
	// Overrides is an ordered list, not a map: first match wins, so two
	// patterns that both match one tool resolve deterministically
	// (a map's iteration order is randomized in Go).
	Overrides []ToolTimeoutOverride `json:"overrides,omitempty"`
}

// ToolTimeoutOverride sets a timeout for tools matching any of Tools —
// glob patterns against the tool's namespaced name, same syntax as
// approvals.require/auto_approve.
type ToolTimeoutOverride struct {
	Tools   []string `json:"tools"`
	Timeout string   `json:"timeout"`
}

// RetryConfig governs automatic retry-with-backoff of a provider (LLM)
// call that fails with a transient error — a rate limit, a momentary
// server or gateway error, or (with OnNetworkError) a dropped connection
// — instead of ending the run outright. See internal/provider.WithRetry
// and internal/agent.retryPolicy, which turns this into the engine's
// actual policy, applying its own defaults for every field left unset
// here. A field's zero value ("", 0, nil) always means "use the engine's
// default", never "off" — MaxAttempts: 1 is how a config opts out of
// retries entirely.
type RetryConfig struct {
	// MaxAttempts is the total number of attempts, including the first;
	// 1 disables retries outright. 0 (the default) uses the engine's
	// built-in default.
	MaxAttempts int `json:"max_attempts,omitempty"`
	// InitialDelay is the backoff before the first retry; each
	// subsequent one roughly doubles (with jitter), up to MaxDelay.
	InitialDelay string `json:"initial_delay,omitempty"`
	// MaxDelay caps a single backoff sleep.
	MaxDelay string `json:"max_delay,omitempty"`
	// MaxElapsed caps the total time spent retrying one provider call,
	// across every attempt. Retries are also implicitly bounded by
	// limits.timeout, if set — whichever deadline is tighter wins, since
	// the retry loop checks the run's own context deadline directly.
	MaxElapsed string `json:"max_elapsed,omitempty"`
	// OnNetworkError, if true, also retries a transport failure (a
	// dropped connection, a DNS failure, ...) that never reached an HTTP
	// response at all, not just an HTTP status. nil uses the engine's
	// default.
	OnNetworkError *bool `json:"on_network_error,omitempty"`
	// OnExhausted is "interrupt" (default) — a run whose retry budget
	// runs out is left in a resumable state (`agentforge runs resume`)
	// rather than failed outright, since the underlying condition may
	// clear on its own — or "fail", matching the behavior from before
	// retries existed.
	OnExhausted string `json:"on_exhausted,omitempty"`
}
