package provider

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// scriptedProvider replays a fixed sequence of outcomes, one per call —
// nil means success. It exists purely to drive WithRetry's decision
// logic; it never talks to a real endpoint.
type scriptedProvider struct {
	outcomes []error
	calls    int
}

func (s *scriptedProvider) Name() string               { return "scripted" }
func (s *scriptedProvider) Capabilities() Capabilities { return Capabilities{} }

func (s *scriptedProvider) Complete(ctx context.Context, r Request) (*Response, error) {
	if s.calls >= len(s.outcomes) {
		return nil, fmt.Errorf("scripted: no more outcomes (call %d)", s.calls)
	}
	err := s.outcomes[s.calls]
	s.calls++
	if err != nil {
		return nil, err
	}
	return &Response{}, nil
}

func (s *scriptedProvider) Stream(ctx context.Context, r Request) (Stream, error) {
	resp, err := s.Complete(ctx, r)
	if err != nil {
		return nil, err
	}
	return NewResponseStream(resp), nil
}

// noSleep is the test seam every test below injects: it never actually
// waits, so the retry loop's decision logic can be exercised without
// tests taking real wall-clock time.
func noSleep(ctx context.Context, d time.Duration) error { return nil }

func TestWithRetrySucceedsAfterRetryableErrors(t *testing.T) {
	sp := &scriptedProvider{outcomes: []error{
		&Error{Provider: "x", StatusCode: 429},
		&Error{Provider: "x", StatusCode: 503},
		nil,
	}}
	pol := RetryPolicy{MaxAttempts: 3, sleep: noSleep}
	p := WithRetry(sp, pol, nil)

	if _, err := p.Complete(context.Background(), Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if sp.calls != 3 {
		t.Errorf("calls = %d, want 3", sp.calls)
	}
}

func TestWithRetryNonRetryableStatusNotRetried(t *testing.T) {
	sp := &scriptedProvider{outcomes: []error{
		&Error{Provider: "x", StatusCode: 400, Message: "bad request"},
		nil, // would succeed if retried — proves it wasn't
	}}
	pol := RetryPolicy{MaxAttempts: 3, sleep: noSleep}
	p := WithRetry(sp, pol, nil)

	_, err := p.Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if sp.calls != 1 {
		t.Errorf("calls = %d, want 1 (a 400 must not be retried)", sp.calls)
	}
	var re *RetriesExhaustedError
	if errors.As(err, &re) {
		t.Errorf("a non-retryable error must not be wrapped as exhausted, got %v", err)
	}
}

func TestWithRetryExhaustionWrapsLastError(t *testing.T) {
	last := &Error{Provider: "x", StatusCode: 503, Message: "unavailable"}
	sp := &scriptedProvider{outcomes: []error{
		&Error{Provider: "x", StatusCode: 503, Message: "unavailable"},
		&Error{Provider: "x", StatusCode: 503, Message: "unavailable"},
		last,
	}}
	pol := RetryPolicy{MaxAttempts: 3, sleep: noSleep}
	p := WithRetry(sp, pol, nil)

	_, err := p.Complete(context.Background(), Request{})
	var re *RetriesExhaustedError
	if !errors.As(err, &re) {
		t.Fatalf("expected *RetriesExhaustedError, got %T: %v", err, err)
	}
	if re.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", re.Attempts)
	}
	var perr *Error
	if !errors.As(re, &perr) || perr.StatusCode != 503 {
		t.Errorf("errors.As did not reach the wrapped *Error with status 503: %v", re.Err)
	}
	if sp.calls != 3 {
		t.Errorf("calls = %d, want 3", sp.calls)
	}
}

func TestWithRetryCancelledDuringBackoffReturnsProviderError(t *testing.T) {
	sp := &scriptedProvider{outcomes: []error{
		&Error{Provider: "x", StatusCode: 503, Message: "unavailable"},
		nil, // never reached
	}}
	cancelledSleep := func(ctx context.Context, d time.Duration) error {
		return context.Canceled
	}
	pol := RetryPolicy{MaxAttempts: 3, sleep: cancelledSleep}
	p := WithRetry(sp, pol, nil)

	_, err := p.Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected an error")
	}
	// Must be the provider error, not context.Canceled — stepModel
	// classifies on ctx.Err(), not on the returned error, so the useful
	// status text must survive into runs.error.
	var perr *Error
	if !errors.As(err, &perr) || perr.StatusCode != 503 {
		t.Errorf("expected the wrapped *Error (status 503) to survive, got %v", err)
	}
	var re *RetriesExhaustedError
	if errors.As(err, &re) {
		t.Errorf("a cancel-during-backoff must not be reported as exhausted, got %v", err)
	}
	if sp.calls != 1 {
		t.Errorf("calls = %d, want 1 (backoff was cut short before a second attempt)", sp.calls)
	}
}

func TestWithRetryStopsBeforeSleepingPastCtxDeadline(t *testing.T) {
	sp := &scriptedProvider{outcomes: []error{
		&Error{Provider: "x", StatusCode: 503, Message: "unavailable"},
		nil, // never reached
	}}
	sleepCalled := false
	pol := RetryPolicy{
		MaxAttempts:  3,
		InitialDelay: 30 * time.Second, // next delay would cross the deadline below
		MaxDelay:     30 * time.Second,
		sleep: func(ctx context.Context, d time.Duration) error {
			sleepCalled = true
			return nil
		},
	}
	p := WithRetry(sp, pol, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := p.Complete(ctx, Request{})
	var re *RetriesExhaustedError
	if !errors.As(err, &re) {
		t.Fatalf("expected *RetriesExhaustedError, got %T: %v", err, err)
	}
	if sleepCalled {
		t.Error("must stop before sleeping past ctx's own deadline, not sleep into it")
	}
	if sp.calls != 1 {
		t.Errorf("calls = %d, want 1", sp.calls)
	}
}

func TestWithRetryClampsAnImplausibleRetryAfter(t *testing.T) {
	var gotDelay time.Duration
	sp := &scriptedProvider{outcomes: []error{
		&Error{Provider: "x", StatusCode: 429, RetryAfter: time.Hour},
		nil,
	}}
	pol := RetryPolicy{
		MaxAttempts: 2,
		MaxDelay:    30 * time.Second,
		sleep: func(ctx context.Context, d time.Duration) error {
			gotDelay = d
			return nil
		},
	}
	p := WithRetry(sp, pol, nil)

	if _, err := p.Complete(context.Background(), Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotDelay > pol.MaxDelay*4 {
		t.Errorf("delay = %v, want it clamped to <= %v (MaxDelay*4)", gotDelay, pol.MaxDelay*4)
	}
}

// TestWithRetryPassesThroughPlainErrors covers the replay provider (and
// runtime's fakeProvider): a plain error, not a *provider.Error, must
// never be retried and must pass through untouched — this is what makes
// gating on the typed error, rather than on status codes, safe for every
// scripted provider in the codebase.
func TestWithRetryPassesThroughPlainErrors(t *testing.T) {
	sp := &scriptedProvider{outcomes: []error{
		errors.New("scripted failure, not a provider.Error"),
		nil,
	}}
	pol := RetryPolicy{MaxAttempts: 3, sleep: noSleep}
	p := WithRetry(sp, pol, nil)

	_, err := p.Complete(context.Background(), Request{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if sp.calls != 1 {
		t.Errorf("calls = %d, want 1 (a plain error must not be retried)", sp.calls)
	}
}

// TestWithRetryMaxAttemptsOneIsPassthrough covers the default (and
// explicit opt-out) case: MaxAttempts <= 1 must behave identically to
// having no decorator at all — no wrapping, no extra call, even for a
// retryable status.
func TestWithRetryMaxAttemptsOneIsPassthrough(t *testing.T) {
	inner := &Error{Provider: "x", StatusCode: 503, Message: "unavailable"}
	sp := &scriptedProvider{outcomes: []error{inner}}
	pol := RetryPolicy{MaxAttempts: 1, sleep: noSleep}
	p := WithRetry(sp, pol, nil)

	_, err := p.Complete(context.Background(), Request{})
	if err != inner {
		t.Errorf("expected the exact same error back unwrapped, got %v (%T)", err, err)
	}
	if sp.calls != 1 {
		t.Errorf("calls = %d, want 1", sp.calls)
	}
}

func TestWithRetryNetworkErrorGatedByOnNetworkError(t *testing.T) {
	transportErr := newTransportError("x", errors.New("connection reset"))

	t.Run("off by default", func(t *testing.T) {
		sp := &scriptedProvider{outcomes: []error{transportErr, nil}}
		pol := RetryPolicy{MaxAttempts: 3, sleep: noSleep}
		p := WithRetry(sp, pol, nil)
		if _, err := p.Complete(context.Background(), Request{}); err == nil {
			t.Fatal("expected an error")
		}
		if sp.calls != 1 {
			t.Errorf("calls = %d, want 1 (OnNetworkError is off)", sp.calls)
		}
	})

	t.Run("on when enabled", func(t *testing.T) {
		sp := &scriptedProvider{outcomes: []error{transportErr, nil}}
		pol := RetryPolicy{MaxAttempts: 3, OnNetworkError: true, sleep: noSleep}
		p := WithRetry(sp, pol, nil)
		if _, err := p.Complete(context.Background(), Request{}); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if sp.calls != 2 {
			t.Errorf("calls = %d, want 2", sp.calls)
		}
	})
}

func TestWithRetryStreamRetriesEstablishmentOnly(t *testing.T) {
	sp := &scriptedProvider{outcomes: []error{
		&Error{Provider: "x", StatusCode: 429},
		nil,
	}}
	pol := RetryPolicy{MaxAttempts: 2, sleep: noSleep}
	p := WithRetry(sp, pol, nil)

	stream, err := p.Stream(context.Background(), Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if sp.calls != 2 {
		t.Errorf("calls = %d, want 2", sp.calls)
	}
	// Once established, the returned Stream is the inner one, unwrapped —
	// no further retry machinery sits between the caller and it.
	if _, ok := stream.(*staticStream); !ok {
		t.Errorf("expected the unwrapped inner Stream, got %T", stream)
	}
}
