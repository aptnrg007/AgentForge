package provider

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestErrorRendersLikeOldFmtErrorf is the guard that makes the switch to
// *Error a provable no-op: every provider's Complete/Stream produced one
// of these two fmt.Errorf shapes before *Error existed, and runtime.go's
// stepModel folds this text into the persisted runs.error column, so a
// rendering change here would silently change what `runs get` and the
// HTTP API's run trace show for an old failure.
func TestErrorRendersLikeOldFmtErrorf(t *testing.T) {
	t.Run("status error", func(t *testing.T) {
		for _, providerName := range []string{"anthropic", "openai", "gemini", "ollama"} {
			e := &Error{Provider: providerName, StatusCode: 429, Message: "rate limited"}
			want := fmt.Sprintf("%s: status %d: %s", providerName, 429, "rate limited")
			if got := e.Error(); got != want {
				t.Errorf("%s: Error() = %q, want %q", providerName, got, want)
			}
		}
	})

	t.Run("transport error", func(t *testing.T) {
		inner := errors.New("connection reset by peer")
		for _, providerName := range []string{"anthropic", "openai", "gemini", "ollama"} {
			e := newTransportError(providerName, inner)
			want := fmt.Sprintf("%s: request failed: %v", providerName, inner)
			if got := e.Error(); got != want {
				t.Errorf("%s: Error() = %q, want %q", providerName, got, want)
			}
		}
	})
}

func TestErrorUnwrap(t *testing.T) {
	inner := errors.New("boom")
	e := newTransportError("anthropic", inner)
	if !errors.Is(e, inner) {
		t.Error("errors.Is did not reach the wrapped transport error")
	}

	// A status error wraps nothing (Err is nil) — Unwrap must return nil,
	// not panic or return itself.
	se := &Error{Provider: "anthropic", StatusCode: 500, Message: "boom"}
	if se.Unwrap() != nil {
		t.Error("status error's Unwrap() should be nil")
	}
}

func TestErrorRetryable(t *testing.T) {
	cases := []struct {
		status int
		want   bool
	}{
		{0, true}, // transport failure
		{408, true},
		{425, true},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true},
		{529, true}, // Anthropic overloaded_error
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{413, false},
		{422, false},
		{200, false},
	}
	for _, c := range cases {
		e := &Error{StatusCode: c.status}
		if got := e.Retryable(); got != c.want {
			t.Errorf("status %d: Retryable() = %v, want %v", c.status, got, c.want)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want time.Duration
	}{
		{"empty", "", 0},
		{"delta seconds", "120", 120 * time.Second},
		{"zero", "0", 0},
		{"negative", "-5", 0},
		{"garbage", "not-a-date", 0},
		{"http-date in the past", "Wed, 21 Oct 2015 07:28:00 GMT", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseRetryAfter(c.in); got != c.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}

	t.Run("http-date in the future", func(t *testing.T) {
		future := time.Now().Add(90 * time.Second).UTC()
		got := parseRetryAfter(future.Format(http.TimeFormat))
		if got <= 0 || got > 91*time.Second {
			t.Errorf("parseRetryAfter(future date) = %v, want ~90s", got)
		}
	})
}

func TestGeminiRetryDelay(t *testing.T) {
	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"no details", `{"error":{"message":"boom"}}`, 0},
		{"retryDelay present", `{"error":{"message":"rate limited","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"17s"}]}}`, 17 * time.Second},
		{"empty retryDelay skipped", `{"error":{"details":[{"retryDelay":""},{"retryDelay":"5s"}]}}`, 5 * time.Second},
		{"malformed json", `not json`, 0},
		{"unparseable duration", `{"error":{"details":[{"retryDelay":"soon"}]}}`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := geminiRetryDelay([]byte(c.body)); got != c.want {
				t.Errorf("geminiRetryDelay(%q) = %v, want %v", c.body, got, c.want)
			}
		})
	}
}

func TestNewStatusErrorRetryAfterHeaderTakesPriorityOverBody(t *testing.T) {
	resp := &http.Response{
		StatusCode: 429,
		Header:     http.Header{"Retry-After": []string{"30"}},
	}
	bodyFn := func([]byte) time.Duration { return 999 * time.Second }
	e := newStatusError("gemini", resp, nil, "rate limited", bodyFn)
	if e.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s (header should win over body)", e.RetryAfter)
	}
}

func TestNewStatusErrorFallsBackToBody(t *testing.T) {
	resp := &http.Response{StatusCode: 429, Header: http.Header{}}
	body := []byte(`{"error":{"details":[{"retryDelay":"17s"}]}}`)
	e := newStatusError("gemini", resp, body, "rate limited", geminiRetryDelay)
	if e.RetryAfter != 17*time.Second {
		t.Errorf("RetryAfter = %v, want 17s from body", e.RetryAfter)
	}
}

func TestNewStatusErrorNilRetryAfterFromBody(t *testing.T) {
	resp := &http.Response{StatusCode: 500, Header: http.Header{}}
	e := newStatusError("anthropic", resp, []byte("boom"), "boom", nil)
	if e.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want 0 with no header and no body hook", e.RetryAfter)
	}
}
