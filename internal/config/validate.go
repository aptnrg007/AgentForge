package config

import (
	"fmt"
	"time"
)

func (c *Config) validate() error {
	if c.Name == "" {
		return fmt.Errorf("name is required")
	}
	if err := c.Model.validate(); err != nil {
		return fmt.Errorf("model: %w", err)
	}

	seen := map[string]bool{}
	for i, m := range c.MCP {
		if m.Name == "" {
			return fmt.Errorf("mcp[%d]: name is required", i)
		}
		if seen[m.Name] {
			return fmt.Errorf("mcp[%d]: duplicate server name %q", i, m.Name)
		}
		seen[m.Name] = true
		if err := m.validate(); err != nil {
			return fmt.Errorf("mcp[%d] (%s): %w", i, m.Name, err)
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
	switch c.Session.Type {
	case "", "none", "sqlite":
	default:
		return fmt.Errorf("session.type: unknown value %q", c.Session.Type)
	}

	return nil
}

func (m ModelConfig) validate() error {
	switch m.Provider {
	case "ollama", "anthropic", "openai":
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
	return nil
}

func (m MCPServer) validate() error {
	switch m.Transport {
	case "stdio":
		if len(m.Command) == 0 {
			return fmt.Errorf("stdio transport requires command")
		}
	case "http":
		if m.URL == "" {
			return fmt.Errorf("http transport requires url")
		}
	case "":
		return fmt.Errorf("transport is required")
	default:
		return fmt.Errorf("unknown transport %q", m.Transport)
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
