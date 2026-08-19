// Package agent assembles a runnable runtime.Engine from a parsed
// config.Config: picks the provider, connects to the configured MCP
// servers through a shared mcp.Registry, and applies the tools: glob
// filter. It's the wiring layer both internal/cli and internal/api sit on,
// so a config doesn't get turned into a running agent two different ways.
package agent

import (
	"context"
	"fmt"
	"time"

	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/provider"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

const (
	defaultMaxTurns = 10
)

// ProviderFactory builds the provider a model config points at. Tests
// substitute a fake here to exercise the full engine/API without a live
// LLM backend.
type ProviderFactory func(model config.ModelConfig) (provider.Provider, error)

func DefaultProviderFactory(model config.ModelConfig) (provider.Provider, error) {
	switch model.Provider {
	case "ollama":
		return provider.NewOllama(model.BaseURL), nil
	case "anthropic":
		return provider.NewAnthropic(model.APIKey, model.BaseURL), nil
	default:
		return nil, fmt.Errorf("model.provider %q not yet supported", model.Provider)
	}
}

// ResolveTools returns cfg's MCP tools, namespaced and filtered by
// cfg.Tools, without building a full engine.
func ResolveTools(ctx context.Context, registry *mcp.Registry, cfg *config.Config) ([]runtime.Tool, error) {
	if len(cfg.MCP) == 0 {
		return nil, nil
	}

	var all []runtime.Tool
	for _, srv := range cfg.MCP {
		if srv.Transport != "stdio" {
			return nil, fmt.Errorf("mcp server %q: transport %q not yet supported", srv.Name, srv.Transport)
		}
		tools, err := registry.Tools(ctx, srv.Name, mcp.ServerConfig{Command: srv.Command, Env: srv.Env})
		if err != nil {
			return nil, fmt.Errorf("mcp server %q: %w", srv.Name, err)
		}
		all = append(all, tools...)
	}

	return config.FilterTools(all, cfg.Tools), nil
}

// Build constructs a fully wired runtime.Engine for cfg.
func Build(ctx context.Context, st *store.Store, registry *mcp.Registry, cfg *config.Config, newProvider ProviderFactory) (*runtime.Engine, error) {
	prov, err := newProvider(cfg.Model)
	if err != nil {
		return nil, err
	}

	maxTurns := cfg.Limits.MaxTurns
	if maxTurns == 0 {
		maxTurns = defaultMaxTurns
	}

	// cfg.Approvals.Timeout is already validated as a parseable duration
	// string by config.Parse, so a parse error here can't happen in
	// practice; treat it as "no timeout" rather than failing the build.
	timeout, _ := time.ParseDuration(cfg.Approvals.Timeout)

	eng := runtime.NewEngine(st, prov, runtime.Config{
		AgentName:   cfg.Name,
		Model:       cfg.Model.Name,
		System:      cfg.Instructions,
		MaxTurns:    maxTurns,
		MaxTokens:   cfg.Limits.MaxTokens,
		Temperature: cfg.Model.Temperature,
		Approvals: runtime.ApprovalPolicy{
			Mode:        cfg.Approvals.Mode,
			Require:     cfg.Approvals.Require,
			AutoApprove: cfg.Approvals.AutoApprove,
			Timeout:     timeout,
			OnTimeout:   cfg.Approvals.OnTimeout,
		},
	})

	tools, err := ResolveTools(ctx, registry, cfg)
	if err != nil {
		return nil, err
	}
	for _, t := range tools {
		eng.RegisterTool(t)
	}

	return eng, nil
}
