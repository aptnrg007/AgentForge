package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agentforge/internal/provider"
)

// flakyAnthropicServer serves a fixed sequence of HTTP status codes, then
// falls back to serving a scripted success response once the sequence is
// exhausted or explicitly overridden — letting a test simulate an outage
// clearing (see TestRetryThenResumeAcrossAnOutage) without needing a
// second httptest.Server.
type flakyAnthropicServer struct {
	mu       sync.Mutex
	statuses []int
	success  map[string]any
	requests int
}

func (f *flakyAnthropicServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		status := 0
		if len(f.statuses) > 0 {
			status = f.statuses[0]
			f.statuses = f.statuses[1:]
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "overloaded_error", "message": "server overloaded"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(f.success)
	}
}

func (f *flakyAnthropicServer) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// setStatuses replaces the outstanding scripted status sequence — the
// "the outage clears" step: an empty slice means every subsequent
// request gets the scripted success response.
func (f *flakyAnthropicServer) setStatuses(statuses []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses = statuses
}

// fastRetryPolicy is a real (non-test-seam) RetryPolicy with delays cut
// to milliseconds so these tests don't actually wait — internal/provider's
// sleep test seam is unexported and this test lives in package runtime,
// so tiny real durations stand in for it here instead.
func fastRetryPolicy(maxAttempts int) provider.RetryPolicy {
	return provider.RetryPolicy{
		MaxAttempts:  maxAttempts,
		InitialDelay: 2 * time.Millisecond,
		MaxDelay:     5 * time.Millisecond,
		MaxElapsed:   time.Second,
	}
}

// TestRetryThenResumeAcrossAnOutage is the thesis test: proof that a run
// whose retry budget runs out on a transient-looking provider error is
// left resumable, not failed outright, and that resuming it later — once
// the condition has cleared — completes normally with the full message
// history intact. This is the property the README's "Resumable. Kill the
// process mid-run and it picks back up exactly where it left off." claim
// didn't actually hold for before retry/StateInterrupted existed: a 503
// used to kill the run permanently even though every byte needed to
// continue was already sitting in SQLite.
func TestRetryThenResumeAcrossAnOutage(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	srv := &flakyAnthropicServer{
		statuses: []int{503, 503, 503},
		success:  anthropicTextResponse("back online"),
	}
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	ap := provider.NewAnthropic("test-key", httpSrv.URL)
	retrying := provider.WithRetry(ap, fastRetryPolicy(3), nil)
	eng := NewEngine(st, retrying, Config{
		AgentName: "test-agent", Model: "claude-sonnet-4-6", MaxTurns: 10,
	})

	runID := "run-outage"
	if err := eng.NewRun(ctx, runID, "hello"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	// One Step call: the retry loop lives entirely inside stepModel's
	// single e.complete call, so all 3 scripted 503s are consumed by one
	// Step, not one Step per attempt.
	state, err := eng.Step(ctx, runID)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != StateInterrupted {
		run, _ := st.GetRun(ctx, runID)
		t.Fatalf("expected interrupted after the retry budget ran out, got %s (error=%v)", state, run.Error)
	}
	if srv.requestCount() != 3 {
		t.Errorf("requests sent = %d, want 3", srv.requestCount())
	}

	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Error == nil || !containsStatus503(*run.Error) {
		t.Errorf("run.Error = %v, want it to name the 503 status", run.Error)
	}
	if run.TurnCount != 0 {
		t.Errorf("TurnCount = %d, want 0 — an interrupted call must not consume turn budget", run.TurnCount)
	}

	// The outage clears.
	srv.setStatuses(nil)

	state, err = eng.Step(ctx, runID)
	if err != nil {
		t.Fatalf("Step (resume): %v", err)
	}
	if state != StateCompleted {
		run, _ := st.GetRun(ctx, runID)
		t.Fatalf("expected the run to resume and complete, got %s (error=%v)", state, run.Error)
	}

	run, err = st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.TurnCount != 1 {
		t.Errorf("TurnCount after completion = %d, want 1 (the resumed call is the run's first real turn)", run.TurnCount)
	}

	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (the original user turn plus the resumed assistant reply), got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Content[0].Text != "hello" {
		t.Errorf("expected the original user message to survive the interruption, got %+v", msgs[0])
	}
}

// TestRetryDoesNotInterruptOnANonRetryableError proves a permanent
// failure (401 — a bad API key, never going to succeed on retry) still
// fails the run outright, with zero retries, rather than being left
// resumable — the same distinction (*Error).Retryable draws at the
// provider layer must hold all the way through to which state the run
// lands in.
func TestRetryDoesNotInterruptOnANonRetryableError(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	srv := &flakyAnthropicServer{
		statuses: []int{401},
		success:  anthropicTextResponse("should not be reached"),
	}
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	ap := provider.NewAnthropic("test-key", httpSrv.URL)
	retrying := provider.WithRetry(ap, fastRetryPolicy(3), nil)
	eng := NewEngine(st, retrying, Config{
		AgentName: "test-agent", Model: "claude-sonnet-4-6", MaxTurns: 10,
	})

	runID := "run-bad-key"
	if err := eng.NewRun(ctx, runID, "hello"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	state, err := eng.Step(ctx, runID)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != StateFailed {
		t.Fatalf("expected a 401 to fail the run outright, got %s", state)
	}
	if srv.requestCount() != 1 {
		t.Errorf("requests sent = %d, want 1 (a 401 must never be retried)", srv.requestCount())
	}
}

// TestOnRetriesExhaustedFailKeepsThePreRetryBehavior proves
// Config.OnRetriesExhausted == "fail" restores the exact pre-retry state
// semantics: exhausting the budget fails the run outright instead of
// leaving it interrupted, for a caller that opted out via retry.on_exhausted.
func TestOnRetriesExhaustedFailKeepsThePreRetryBehavior(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	srv := &flakyAnthropicServer{
		statuses: []int{503, 503, 503},
		success:  anthropicTextResponse("unreachable"),
	}
	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()

	ap := provider.NewAnthropic("test-key", httpSrv.URL)
	retrying := provider.WithRetry(ap, fastRetryPolicy(3), nil)
	eng := NewEngine(st, retrying, Config{
		AgentName: "test-agent", Model: "claude-sonnet-4-6", MaxTurns: 10,
		OnRetriesExhausted: "fail",
	})

	runID := "run-opt-out"
	if err := eng.NewRun(ctx, runID, "hello"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	state, err := eng.Step(ctx, runID)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != StateFailed {
		t.Fatalf("expected on_exhausted: fail to fail the run outright, got %s", state)
	}
}

func containsStatus503(s string) bool {
	return strings.Contains(s, "503")
}

// flakyStreamProvider fails Stream's first call with a retryable
// *provider.Error, then succeeds and streams a scripted sequence of
// deltas — standing in for a provider whose stream endpoint 503s once
// before establishing. Complete is never expected to be called (an event
// sink is always installed in the tests that use this).
type flakyStreamProvider struct {
	t           *testing.T
	failures    int
	streamCalls int
	deltas      []string
	resp        *provider.Response
}

func (p *flakyStreamProvider) Name() string { return "flaky-stream-fake" }

func (p *flakyStreamProvider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	p.t.Fatal("Complete must not be called when an event sink is installed")
	return nil, nil
}

func (p *flakyStreamProvider) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	p.streamCalls++
	if p.streamCalls <= p.failures {
		return nil, &provider.Error{Provider: p.Name(), StatusCode: 503, Message: "server overloaded"}
	}
	return &deltaStream{deltas: p.deltas, resp: p.resp}, nil
}

func (p *flakyStreamProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

// TestStreamRetriesEstablishmentOnlyNoDuplicateTokens is the runtime-level
// counterpart to internal/provider's TestWithRetryStreamRetriesEstablishmentOnly:
// a stream that fails to establish once (a 503 before any token) retries
// and succeeds, and every delta the caller's OnEvent callback observes
// appears exactly once — proving the retry decorator never touches a
// stream once one has actually been handed back, which is what makes
// retrying a stream establishment safe even though retrying mid-stream
// would not be (see provider.WithRetry's Stream doc comment).
func TestStreamRetriesEstablishmentOnlyNoDuplicateTokens(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))

	fp := &flakyStreamProvider{
		t: t, failures: 1,
		deltas: []string{"Hel", "lo, ", "world"},
		resp:   textResponse("Hello, world"),
	}
	retrying := provider.WithRetry(fp, fastRetryPolicy(3), nil)
	eng := NewEngine(st, retrying, Config{AgentName: "test-agent", Model: "test-model", MaxTurns: 10})

	var tokens []string
	eng.OnEvent(func(ev Event) {
		if ev.Kind == EventToken {
			tokens = append(tokens, ev.Text)
		}
	})

	runID := "run-flaky-stream"
	if err := eng.NewRun(ctx, runID, "hi"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	state, err := eng.Step(ctx, runID)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != StateCompleted {
		t.Fatalf("expected completed, got %s", state)
	}
	if fp.streamCalls != 2 {
		t.Errorf("Stream calls = %d, want 2 (1 failure + 1 success)", fp.streamCalls)
	}
	want := []string{"Hel", "lo, ", "world"}
	if len(tokens) != len(want) {
		t.Fatalf("tokens = %v, want %v (no duplicates from the retried establishment)", tokens, want)
	}
	for i := range want {
		if tokens[i] != want[i] {
			t.Errorf("tokens[%d] = %q, want %q", i, tokens[i], want[i])
		}
	}
}
