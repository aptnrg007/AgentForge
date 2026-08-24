package config

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"text/template"
	"time"

	"agentforge/internal/schema"
)

func (c *Config) validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if err := c.Model.validate(); err != nil {
		return fmt.Errorf("model: %w", err)
	}

	mcpNamespaces := map[string]bool{}
	for i, m := range c.MCP {
		if m.Name == "" {
			return fmt.Errorf("mcp[%d]: name is required", i)
		}
		if mcpNamespaces[m.Name] {
			return fmt.Errorf("mcp[%d]: duplicate server name %q", i, m.Name)
		}
		mcpNamespaces[m.Name] = true
		if err := m.validate(); err != nil {
			return fmt.Errorf("mcp[%d] (%s): %w", i, m.Name, err)
		}
	}

	toolDefNames := map[string]bool{}
	for i, td := range c.ToolDefinitions {
		if err := td.validate(); err != nil {
			return fmt.Errorf("tool_definitions[%d]: %w", i, err)
		}
		if toolDefNames[td.Name] {
			return fmt.Errorf("tool_definitions[%d]: duplicate name %q", i, td.Name)
		}
		toolDefNames[td.Name] = true
		if dot := strings.IndexByte(td.Name, '.'); dot >= 0 && mcpNamespaces[td.Name[:dot]] {
			return fmt.Errorf("tool_definitions[%d]: name %q collides with mcp server %q's namespace", i, td.Name, td.Name[:dot])
		}
		if len(c.Tools) > 0 {
			matched := false
			for _, pattern := range c.Tools {
				if ok, _ := path.Match(pattern, td.Name); ok {
					matched = true
					break
				}
			}
			if !matched {
				return fmt.Errorf("tool_definitions[%d] (%s): excluded by the tools: filter — add %q to tools", i, td.Name, td.Name)
			}
		}
	}

	for i, pattern := range c.Tools {
		if pattern == "" {
			return fmt.Errorf("tools[%d]: pattern is empty", i)
		}
	}

	if err := c.Approvals.validate(); err != nil {
		return fmt.Errorf("approvals: %w", err)
	}
	if err := c.Limits.validate(); err != nil {
		return fmt.Errorf("limits: %w", err)
	}
	if err := c.Output.validate(); err != nil {
		return fmt.Errorf("output: %w", err)
	}
	if err := c.ToolPolicy.validate(); err != nil {
		return fmt.Errorf("tool_policy: %w", err)
	}
	if err := c.Retry.validate(); err != nil {
		return fmt.Errorf("retry: %w", err)
	}
	return nil
}

func (m ModelConfig) validate() error {
	switch m.Provider {
	case "ollama", "anthropic", "openai", "gemini":
	case "":
		return fmt.Errorf("provider is required")
	default:
		return fmt.Errorf("unknown provider %q", m.Provider)
	}
	if m.Name == "" {
		return fmt.Errorf("name is required")
	}
	// This runs after env interpolation (load.go), so an api_key: ${VAR}
	// reference to a missing variable has already failed with a clearer
	// error by this point — this only catches api_key being left out of
	// the YAML entirely, fast at load time rather than as a 401 on the
	// first request.
	if m.Provider == "anthropic" && m.APIKey == "" {
		return fmt.Errorf("api_key is required for provider \"anthropic\"")
	}
	// gemini always requires a key too — there is no self-hosted or
	// no-auth equivalent the way openai's base_url exemption covers.
	if m.Provider == "gemini" && m.APIKey == "" {
		return fmt.Errorf("api_key is required for provider \"gemini\"")
	}
	// openai only requires a key when base_url is empty (real OpenAI). A
	// base_url pointing at a local/self-hosted OpenAI-compatible server
	// (vLLM, llama.cpp, LM Studio) typically ignores auth entirely, and
	// requiring a key there would make "point it at your local server"
	// need a fake credential for no reason.
	if m.Provider == "openai" && m.APIKey == "" && m.BaseURL == "" {
		return fmt.Errorf("api_key is required for provider \"openai\" (unless base_url points at a server that doesn't need one)")
	}
	return nil
}

func (m MCPServer) validate() error {
	switch m.Transport {
	case "stdio":
		if len(m.Command) == 0 {
			return fmt.Errorf("stdio transport requires command")
		}
	case "http":
		// Ground rule 3 (fail at load time, not use time): building an
		// engine used to accept this at config-validation time and only
		// reject it later, in agent.Build, once a run tried to actually
		// connect — see docs/DESIGN.md. Reject it here instead, so a typo'd
		// or aspirational "transport: http" fails immediately.
		return fmt.Errorf("transport \"http\" is not yet supported (only \"stdio\" is)")
	case "":
		return fmt.Errorf("transport is required")
	default:
		return fmt.Errorf("unknown transport %q", m.Transport)
	}
	return nil
}

var toolDefNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func (td ToolDefinition) validate() error {
	if td.Name == "" {
		return fmt.Errorf("name is required")
	}
	if !toolDefNamePattern.MatchString(td.Name) {
		return fmt.Errorf("name %q: must match %s (no glob metacharacters — it has to be matchable by its own tools: pattern)", td.Name, toolDefNamePattern.String())
	}
	if td.Description == "" {
		return fmt.Errorf("description is required")
	}
	if len(td.InputSchema) == 0 {
		return fmt.Errorf("input_schema is required")
	}
	if _, err := schema.Compile(td.InputSchema); err != nil {
		return fmt.Errorf("input_schema: %w", err)
	}

	if td.HTTP == nil && td.Command == nil {
		return fmt.Errorf("exactly one of http/command is required")
	}
	if td.HTTP != nil && td.Command != nil {
		return fmt.Errorf("only one of http/command may be set")
	}
	if td.HTTP != nil {
		if err := td.HTTP.validate(); err != nil {
			return fmt.Errorf("http: %w", err)
		}
	}
	if td.Command != nil {
		if err := td.Command.validate(); err != nil {
			return fmt.Errorf("command: %w", err)
		}
	}
	return nil
}

func (h HTTPToolConfig) validate() error {
	if h.URL == "" {
		return fmt.Errorf("url is required")
	}
	if err := validateTemplate("url", h.URL); err != nil {
		return err
	}
	// The host must be literal: a placeholder there would let the model
	// retarget the request at an arbitrary server. Placeholders in the
	// path (e.g. "https://api.example.com/{{.id}}") are fine. This
	// checks the raw, unrendered URL string directly rather than going
	// through url.Parse — url.Parse rejects "{" in a host outright, so
	// it can't be used to *find* the problem, only to trip over it.
	if err := requireLiteralHost(h.URL); err != nil {
		return err
	}
	switch h.Method {
	case "", "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
	default:
		return fmt.Errorf("method: unknown value %q", h.Method)
	}
	if h.MaxResponseBytes < 0 {
		return fmt.Errorf("max_response_bytes: must be >= 0")
	}
	for k, v := range h.Query {
		if err := validateTemplate("query["+k+"]", v); err != nil {
			return err
		}
	}
	for k, v := range h.Headers {
		if err := validateTemplate("headers["+k+"]", v); err != nil {
			return err
		}
	}
	if err := validateTemplate("body", h.Body); err != nil {
		return err
	}
	return nil
}

// requireLiteralHost rejects a URL template whose scheme://authority
// portion contains a {{ }} placeholder.
func requireLiteralHost(rawURL string) error {
	idx := strings.Index(rawURL, "://")
	if idx < 0 {
		return fmt.Errorf("url: must be an absolute URL (missing scheme://)")
	}
	rest := rawURL[idx+3:]
	authority := rest
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		authority = rest[:end]
	}
	if strings.Contains(authority, "{{") {
		return fmt.Errorf("url: scheme and host must be literal (no {{ }} template) — put placeholders in the path or query instead")
	}
	return nil
}

func (c CommandToolConfig) validate() error {
	if len(c.Argv) == 0 || c.Argv[0] == "" {
		return fmt.Errorf("argv: must have at least one element, with argv[0] non-empty")
	}
	// argv[0] chooses the binary; that has to stay literal, or the model
	// could point exec at anything on PATH.
	if strings.Contains(c.Argv[0], "{{") {
		return fmt.Errorf("argv[0]: must be literal (no {{ }} template) — the model may supply arguments, not the binary")
	}
	for i, a := range c.Argv {
		if err := validateTemplate(fmt.Sprintf("argv[%d]", i), a); err != nil {
			return err
		}
	}
	if err := validateTemplate("workdir", c.Workdir); err != nil {
		return err
	}
	for k, v := range c.Env {
		if err := validateTemplate("env["+k+"]", v); err != nil {
			return err
		}
	}
	if err := validateTemplate("stdin", c.Stdin); err != nil {
		return err
	}
	if c.MaxOutputBytes < 0 {
		return fmt.Errorf("max_output_bytes: must be >= 0")
	}
	return nil
}

// validateTemplate parses s as a text/template without executing it, the
// same missingkey=error dialect internal/tools uses at call time — this
// only catches a malformed {{ }} at load time, not a placeholder the
// input never supplies (that's a runtime error, by design).
func validateTemplate(field, s string) error {
	if s == "" {
		return nil
	}
	if _, err := template.New(field).Parse(s); err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	return nil
}

func (a ApprovalsConfig) validate() error {
	switch a.Mode {
	case "", "never", "annotated", "always":
	default:
		return fmt.Errorf("mode: unknown value %q", a.Mode)
	}
	switch a.OnTimeout {
	case "", "deny", "allow":
	default:
		return fmt.Errorf("on_timeout: unknown value %q", a.OnTimeout)
	}
	if a.Timeout != "" {
		if _, err := time.ParseDuration(a.Timeout); err != nil {
			return fmt.Errorf("timeout: %w", err)
		}
	}
	return nil
}

func (l LimitsConfig) validate() error {
	if l.MaxTurns < 0 {
		return fmt.Errorf("max_turns: must be >= 0")
	}
	if l.MaxTokens < 0 {
		return fmt.Errorf("max_tokens: must be >= 0")
	}
	if l.Timeout != "" {
		if _, err := time.ParseDuration(l.Timeout); err != nil {
			return fmt.Errorf("timeout: %w", err)
		}
	}
	return nil
}

func (o OutputConfig) validate() error {
	switch o.OnInvalid {
	case "", "retry", "fail":
	default:
		return fmt.Errorf("on_invalid: unknown value %q", o.OnInvalid)
	}
	if o.MaxRetries < 0 {
		return fmt.Errorf("max_retries: must be >= 0")
	}
	if o.Schema == "" && (o.OnInvalid != "" || o.MaxRetries != 0) {
		return fmt.Errorf("on_invalid/max_retries set without schema")
	}
	return nil
}

func (p ToolPolicyConfig) validate() error {
	switch p.OnTimeout {
	case "", "error", "fail":
	default:
		return fmt.Errorf("on_timeout: unknown value %q", p.OnTimeout)
	}
	if p.Timeout == "" && (p.OnTimeout != "" || len(p.Overrides) > 0) {
		return fmt.Errorf("on_timeout/overrides set without timeout")
	}
	if p.Timeout != "" {
		if err := validatePositiveDuration(p.Timeout); err != nil {
			return fmt.Errorf("timeout: %w", err)
		}
	}
	for i, o := range p.Overrides {
		if len(o.Tools) == 0 {
			return fmt.Errorf("overrides[%d]: tools is required", i)
		}
		for j, pattern := range o.Tools {
			if pattern == "" {
				return fmt.Errorf("overrides[%d].tools[%d]: pattern is empty", i, j)
			}
		}
		if err := validatePositiveDuration(o.Timeout); err != nil {
			return fmt.Errorf("overrides[%d].timeout: %w", i, err)
		}
	}
	return nil
}

// validate checks r in isolation. Deliberately not an error: MaxElapsed
// left unconstrained relative to limits.timeout — the retry loop reads
// the run's own context deadline directly (see internal/provider/retry.go),
// so the two can never actually conflict at runtime, and for a run
// resumed later the relationship between them is legitimately different
// anyway.
func (r RetryConfig) validate() error {
	if r.MaxAttempts < 0 || r.MaxAttempts > 20 {
		return fmt.Errorf("max_attempts: must be between 0 and 20, got %d", r.MaxAttempts)
	}
	if r.InitialDelay != "" {
		if err := validatePositiveDuration(r.InitialDelay); err != nil {
			return fmt.Errorf("initial_delay: %w", err)
		}
	}
	if r.MaxDelay != "" {
		if err := validatePositiveDuration(r.MaxDelay); err != nil {
			return fmt.Errorf("max_delay: %w", err)
		}
	}
	if r.MaxElapsed != "" {
		if err := validatePositiveDuration(r.MaxElapsed); err != nil {
			return fmt.Errorf("max_elapsed: %w", err)
		}
	}
	if r.InitialDelay != "" && r.MaxDelay != "" {
		initial, _ := time.ParseDuration(r.InitialDelay)
		max, _ := time.ParseDuration(r.MaxDelay)
		if initial > max {
			return fmt.Errorf("initial_delay (%s) must be <= max_delay (%s)", r.InitialDelay, r.MaxDelay)
		}
	}
	switch r.OnExhausted {
	case "", "interrupt", "fail":
	default:
		return fmt.Errorf("on_exhausted: unknown value %q", r.OnExhausted)
	}
	return nil
}

func validatePositiveDuration(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if d <= 0 {
		return fmt.Errorf("must be > 0, got %q", s)
	}
	return nil
}
