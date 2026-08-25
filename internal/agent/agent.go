// Package agent assembles a runnable runtime.Engine from a parsed
// config.Config: picks the provider, connects to the configured MCP
// servers through a shared mcp.Registry, and applies the tools: glob
// filter. It's the wiring layer both internal/cli and internal/api sit on,
// so a config doesn't get turned into a running agent two different ways.
package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agentforge/internal/config"
	"agentforge/internal/mcp"
	"agentforge/internal/provider"
	"agentforge/internal/runtime"
	"agentforge/internal/schema"
	"agentforge/internal/store"
	"agentforge/internal/tools"
)

const (
	defaultMaxTurns = 10

	// Retry defaults, applied by retryPolicy to any field a config leaves
	// unset. A provider call is the only externally-observable action in
	// the run loop with no side effects (tools only execute after a
	// separate persisted ready_for_tools transition), so retrying by
	// default costs at most a duplicate model call's tokens — not a
	// duplicate tool execution — which is what makes retrying on by
	// default a defensible choice rather than a conservative opt-in.
	// "No retries against a rate-limited API" was a bug in a system
	// whose headline feature is durability, not a safe default to leave
	// silently in place.
	defaultRetryMaxAttempts  = 3
	defaultRetryInitialDelay = time.Second
	defaultRetryMaxDelay     = 30 * time.Second
	defaultRetryMaxElapsed   = 2 * time.Minute
	defaultRetryOnNetworkErr = true
	defaultRetryOnExhausted  = "interrupt"
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
	case "openai":
		return provider.NewOpenAI(model.APIKey, model.BaseURL), nil
	case "gemini":
		return provider.NewGemini(model.APIKey, model.BaseURL), nil
	default:
		return nil, fmt.Errorf("model.provider %q not yet supported", model.Provider)
	}
}

// ResolveTools returns cfg's tools — those declared directly via
// cfg.ToolDefinitions plus cfg.MCP's, namespaced — filtered by cfg.Tools,
// without building a full engine. A config with definitions but no mcp:
// servers at all is the common case for tools.Build's output, so this
// deliberately doesn't short-circuit on len(cfg.MCP) == 0 the way the
// MCP-only loop below would on its own.
func ResolveTools(ctx context.Context, registry *mcp.Registry, cfg *config.Config) ([]runtime.Tool, error) {
	all, err := tools.Build(cfg)
	if err != nil {
		return nil, err
	}

	for _, srv := range cfg.MCP {
		if srv.Transport != "stdio" {
			return nil, fmt.Errorf("mcp server %q: transport %q not yet supported", srv.Name, srv.Transport)
		}
		discovered, err := registry.Tools(ctx, srv.Name, mcp.ServerConfig{Command: srv.Command, Env: srv.Env})
		if err != nil {
			return nil, fmt.Errorf("mcp server %q: %w", srv.Name, err)
		}
		all = append(all, discovered...)
	}

	return config.FilterTools(all, cfg.Tools), nil
}

// Build constructs a fully wired runtime.Engine for cfg.
func Build(ctx context.Context, st *store.Store, registry *mcp.Registry, cfg *config.Config, newProvider ProviderFactory) (*runtime.Engine, error) {
	prov, err := newProvider(cfg.Model)
	if err != nil {
		return nil, err
	}
	prov = provider.WithRetry(prov, retryPolicy(cfg.Retry), nil)

	maxTurns := cfg.Limits.MaxTurns
	if maxTurns == 0 {
		maxTurns = defaultMaxTurns
	}

	// cfg.Approvals.Timeout and cfg.Limits.Timeout are already validated
	// as parseable duration strings by config.Parse, so a parse error
	// here can't happen in practice; treat either as "no timeout" rather
	// than failing the build.
	timeout, _ := time.ParseDuration(cfg.Approvals.Timeout)
	runTimeout, _ := time.ParseDuration(cfg.Limits.Timeout)

	outputPolicy, systemSuffix, err := compileOutputPolicy(cfg, prov)
	if err != nil {
		return nil, err
	}

	eng := runtime.NewEngine(st, prov, runtime.Config{
		AgentName:   cfg.Name,
		Model:       cfg.Model.Name,
		System:      cfg.Instructions + systemSuffix,
		MaxTurns:    maxTurns,
		MaxTokens:   cfg.Limits.MaxTokens,
		Temperature: cfg.Model.Temperature,
		NumCtx:      cfg.Model.NumCtx,
		Think:       cfg.Model.Think,
		Approvals: runtime.ApprovalPolicy{
			Mode:        cfg.Approvals.Mode,
			Require:     cfg.Approvals.Require,
			AutoApprove: cfg.Approvals.AutoApprove,
			Timeout:     timeout,
			OnTimeout:   cfg.Approvals.OnTimeout,
		},
		Output:             outputPolicy,
		ToolPolicy:         toolPolicy(cfg.ToolPolicy),
		RunTimeout:         runTimeout,
		OnRetriesExhausted: onRetriesExhausted(cfg.Retry),
	})

	tools, err := ResolveTools(ctx, registry, cfg)
	if err != nil {
		return nil, err
	}
	for _, t := range tools {
		if err := eng.RegisterTool(t); err != nil {
			return nil, err
		}
	}

	return eng, nil
}

// compileOutputPolicy reads and compiles cfg.Output.Schema, if set, into
// a runtime.OutputPolicy plus the system-prompt suffix that tells the
// model a schema is being enforced. Returns a zero OutputPolicy and an
// empty suffix when no schema is configured.
//
// This lives in Build, not internal/config, deliberately: config.Parse/
// Load are filesystem-pure beyond their own YAML file, whereas Build
// already does I/O (MCP connections) and already turns config structs
// into runtime policy structs (see the Approvals block above it).
func compileOutputPolicy(cfg *config.Config, prov provider.Provider) (runtime.OutputPolicy, string, error) {
	if cfg.Output.Schema == "" {
		return runtime.OutputPolicy{}, "", nil
	}

	path := cfg.Output.Schema
	if !filepath.IsAbs(path) && cfg.SourceDir != "" {
		path = filepath.Join(cfg.SourceDir, path)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		resolved, absErr := filepath.Abs(path)
		if absErr != nil {
			resolved = path
		}
		hint := ""
		if cfg.SourceDir == "" {
			hint = " — this run was reconstructed from a config with no source directory (e.g. stored YAML replayed by the daemon or a resumed run), so a relative output.schema path resolves against the process's working directory instead of the original config file's; use an absolute path, or run from the config file's directory"
		}
		return runtime.OutputPolicy{}, "", fmt.Errorf("output.schema: %s: %w (resolved to %s)%s", cfg.Output.Schema, err, resolved, hint)
	}

	validator, err := schema.Compile(raw)
	if err != nil {
		return runtime.OutputPolicy{}, "", fmt.Errorf("output.schema: %s: %w", cfg.Output.Schema, err)
	}

	policy := runtime.OutputPolicy{
		Validate: func(instance []byte) []string {
			extracted, err := schema.ExtractJSON(string(instance))
			if err != nil {
				return []string{err.Error()}
			}
			return validator.Validate(extracted)
		},
		Schema:     validator.Raw(),
		OnInvalid:  cfg.Output.OnInvalid,
		MaxRetries: cfg.Output.MaxRetries,
		Native:     prov.Capabilities().StructuredOutput,
	}
	return policy, schema.Instruction(validator.Raw()), nil
}

// toolPolicy translates a validated config.ToolPolicyConfig into the
// runtime's ToolPolicy. Every duration here is re-parsed with the error
// dropped, exactly like the Approvals.Timeout parse above in Build: by
// this point config.Parse has already validated every duration string in
// cfg.ToolPolicy, so a parse error is unreachable in practice, and
// treating an (unreachable) failure as "unbounded" is the safe default.
func toolPolicy(cfg config.ToolPolicyConfig) runtime.ToolPolicy {
	timeout, _ := time.ParseDuration(cfg.Timeout)

	overrides := make([]runtime.ToolTimeoutRule, len(cfg.Overrides))
	for i, o := range cfg.Overrides {
		d, _ := time.ParseDuration(o.Timeout)
		overrides[i] = runtime.ToolTimeoutRule{Patterns: o.Tools, Timeout: d}
	}

	return runtime.ToolPolicy{
		Timeout:   timeout,
		OnTimeout: cfg.OnTimeout,
		Overrides: overrides,
	}
}

// retryPolicy translates a validated config.RetryConfig into the
// provider package's actual retry policy, same re-parse-the-validated-
// duration pattern as toolPolicy above: by this point config.Parse has
// already validated every duration string in cfg.Retry, so a parse
// failure here is unreachable in practice, and treating one as "use the
// default" is the safe fallback. A zero value in cfg (0, "", nil) always
// means "use the engine's default" here — see RetryConfig's doc comment —
// which is what lets an absent retry: block resolve to MaxAttempts: 1,
// the passthrough case provider.WithRetry treats as a strict no-op.
func retryPolicy(cfg config.RetryConfig) provider.RetryPolicy {
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultRetryMaxAttempts
	}

	initialDelay := defaultRetryInitialDelay
	if d, err := time.ParseDuration(cfg.InitialDelay); err == nil && d > 0 {
		initialDelay = d
	}
	maxDelay := defaultRetryMaxDelay
	if d, err := time.ParseDuration(cfg.MaxDelay); err == nil && d > 0 {
		maxDelay = d
	}
	maxElapsed := defaultRetryMaxElapsed
	if d, err := time.ParseDuration(cfg.MaxElapsed); err == nil && d > 0 {
		maxElapsed = d
	}
	onNetworkError := defaultRetryOnNetworkErr
	if cfg.OnNetworkError != nil {
		onNetworkError = *cfg.OnNetworkError
	}

	return provider.RetryPolicy{
		MaxAttempts:    maxAttempts,
		InitialDelay:   initialDelay,
		MaxDelay:       maxDelay,
		MaxElapsed:     maxElapsed,
		OnNetworkError: onNetworkError,
	}
}

// onRetriesExhausted resolves cfg.OnExhausted to the runtime's terminal-
// vs-resumable choice for a run whose retry budget runs out: "interrupt"
// (the default — see RetryConfig.OnExhausted) or "fail".
func onRetriesExhausted(cfg config.RetryConfig) string {
	if cfg.OnExhausted == "" {
		return defaultRetryOnExhausted
	}
	return cfg.OnExhausted
}
