package agent

import (
	"context"
	"fmt"
	"testing"
	"time"

	"agentforge/internal/config"
	"agentforge/internal/provider"
	"agentforge/internal/runtime"
)

func TestRetryPolicyDefaults(t *testing.T) {
	pol := retryPolicy(config.RetryConfig{})
	if pol.MaxAttempts != defaultRetryMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", pol.MaxAttempts, defaultRetryMaxAttempts)
	}
	if pol.InitialDelay != defaultRetryInitialDelay {
		t.Errorf("InitialDelay = %v, want %v", pol.InitialDelay, defaultRetryInitialDelay)
	}
	if pol.MaxDelay != defaultRetryMaxDelay {
		t.Errorf("MaxDelay = %v, want %v", pol.MaxDelay, defaultRetryMaxDelay)
	}
	if pol.MaxElapsed != defaultRetryMaxElapsed {
		t.Errorf("MaxElapsed = %v, want %v", pol.MaxElapsed, defaultRetryMaxElapsed)
	}
	if pol.OnNetworkError != defaultRetryOnNetworkErr {
		t.Errorf("OnNetworkError = %v, want %v", pol.OnNetworkError, defaultRetryOnNetworkErr)
	}
}

func TestRetryPolicyRespectsExplicitValues(t *testing.T) {
	onNetErr := true
	pol := retryPolicy(config.RetryConfig{
		MaxAttempts:    5,
		InitialDelay:   "2s",
		MaxDelay:       "1m",
		MaxElapsed:     "5m",
		OnNetworkError: &onNetErr,
	})
	if pol.MaxAttempts != 5 {
		t.Errorf("MaxAttempts = %d, want 5", pol.MaxAttempts)
	}
	if pol.InitialDelay != 2*time.Second {
		t.Errorf("InitialDelay = %v, want 2s", pol.InitialDelay)
	}
	if pol.MaxDelay != time.Minute {
		t.Errorf("MaxDelay = %v, want 1m", pol.MaxDelay)
	}
	if pol.MaxElapsed != 5*time.Minute {
		t.Errorf("MaxElapsed = %v, want 5m", pol.MaxElapsed)
	}
	if !pol.OnNetworkError {
		t.Error("OnNetworkError = false, want true")
	}
}

func TestOnRetriesExhaustedDefaultsToInterrupt(t *testing.T) {
	if got := onRetriesExhausted(config.RetryConfig{}); got != "interrupt" {
		t.Errorf("onRetriesExhausted(zero value) = %q, want %q", got, "interrupt")
	}
}

func TestOnRetriesExhaustedRespectsFail(t *testing.T) {
	if got := onRetriesExhausted(config.RetryConfig{OnExhausted: "fail"}); got != "fail" {
		t.Errorf("onRetriesExhausted(fail) = %q, want %q", got, "fail")
	}
}

// retryScriptedProvider replays a fixed sequence of outcomes — an error
// or, at the end, a success response — standing in for a provider that
// fails transiently a few times before recovering.
type retryScriptedProvider struct {
	errs  []error
	resp  *provider.Response
	calls int
}

func (p *retryScriptedProvider) Name() string                        { return "retry-scripted" }
func (p *retryScriptedProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

func (p *retryScriptedProvider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	if p.calls >= len(p.errs) {
		return nil, fmt.Errorf("retry-scripted provider: no more outcomes")
	}
	err := p.errs[p.calls]
	p.calls++
	if err != nil {
		return nil, err
	}
	return p.resp, nil
}

func (p *retryScriptedProvider) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	resp, err := p.Complete(ctx, r)
	if err != nil {
		return nil, err
	}
	return provider.NewResponseStream(resp), nil
}

// TestBuildWiresRetryConfig proves the config -> agent.Build ->
// provider.WithRetry wiring actually connects, the same way
// TestBuildWiresToolPolicyTimeout proves tool_policy's wiring: a run
// whose first two model calls fail with a retryable status still
// completes, because retry: in the config reached the provider agent.Build
// constructed.
func TestBuildWiresRetryConfig(t *testing.T) {
	st, registry := newTestStoreAndRegistry(t)
	sp := &retryScriptedProvider{
		errs: []error{
			&provider.Error{Provider: "x", StatusCode: 503, Message: "unavailable"},
			&provider.Error{Provider: "x", StatusCode: 503, Message: "unavailable"},
			nil,
		},
		resp: textResp("worked after retries"),
	}
	cfg := &config.Config{
		Name:  "a",
		Model: config.ModelConfig{Provider: "ollama", Name: "m"},
		Retry: config.RetryConfig{MaxAttempts: 3, InitialDelay: "1ms", MaxDelay: "2ms"},
	}
	eng, err := Build(context.Background(), st, registry, cfg, fakeFactory(sp))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	if err := eng.NewRun(ctx, "run-1", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	state, err := eng.Step(ctx, "run-1")
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != runtime.StateCompleted {
		run, _ := st.GetRun(ctx, "run-1")
		t.Fatalf("expected the run to survive the wired retries and complete, got %s (error=%v)", state, run.Error)
	}
	if sp.calls != 3 {
		t.Errorf("calls = %d, want 3 (2 failures + 1 success)", sp.calls)
	}
}

// TestBuildWithNoRetryConfigUsesTheOnByDefaultPolicy covers the current
// default (defaultRetryMaxAttempts == 3, applied whenever a config
// leaves retry: unset): a run whose first model call hits a retryable
// status must still complete on its own, with no retry: block written at
// all — the on-by-default choice, not an opt-in.
func TestBuildWithNoRetryConfigUsesTheOnByDefaultPolicy(t *testing.T) {
	st, registry := newTestStoreAndRegistry(t)
	sp := &retryScriptedProvider{
		errs: []error{
			&provider.Error{Provider: "x", StatusCode: 503, Message: "unavailable"},
			nil,
		},
		resp: textResp("worked via the on-by-default policy"),
	}
	cfg := &config.Config{
		Name:  "a",
		Model: config.ModelConfig{Provider: "ollama", Name: "m"},
	}
	eng, err := Build(context.Background(), st, registry, cfg, fakeFactory(sp))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	if err := eng.NewRun(ctx, "run-1", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	state, err := eng.Step(ctx, "run-1")
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != runtime.StateCompleted {
		run, _ := st.GetRun(ctx, "run-1")
		t.Fatalf("expected the run to survive the default-on retry policy and complete, got %s (error=%v)", state, run.Error)
	}
	if sp.calls != 2 {
		t.Errorf("calls = %d, want 2 (1 failure + 1 success)", sp.calls)
	}
}

// TestBuildWithMaxAttemptsOneOptsOutOfRetries proves an agent can still
// restore the pre-retry behavior exactly, now that it's the one opt-out
// rather than the default: retry.max_attempts: 1 must fail on the first
// transient error, same as before retries existed at all.
func TestBuildWithMaxAttemptsOneOptsOutOfRetries(t *testing.T) {
	st, registry := newTestStoreAndRegistry(t)
	sp := &retryScriptedProvider{
		errs: []error{
			&provider.Error{Provider: "x", StatusCode: 503, Message: "unavailable"},
			nil, // would succeed if retried — proves it wasn't
		},
		resp: textResp("should not be reached"),
	}
	cfg := &config.Config{
		Name:  "a",
		Model: config.ModelConfig{Provider: "ollama", Name: "m"},
		Retry: config.RetryConfig{MaxAttempts: 1},
	}
	eng, err := Build(context.Background(), st, registry, cfg, fakeFactory(sp))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	if err := eng.NewRun(ctx, "run-1", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	state, err := eng.Step(ctx, "run-1")
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != runtime.StateFailed {
		t.Fatalf("expected retry.max_attempts: 1 to restore fail-on-first-error, got %s", state)
	}
	if sp.calls != 1 {
		t.Errorf("calls = %d, want 1 (max_attempts: 1 disables retries)", sp.calls)
	}
}
