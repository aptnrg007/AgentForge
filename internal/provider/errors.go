package provider

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Error is a typed provider failure: either a non-2xx HTTP response
// (StatusCode != 0) or a transport failure before any response was
// received (StatusCode == 0, Err set). It exists so a caller — the retry
// decorator in retry.go — can classify a failure without string-matching
// Error()'s text, while Error() itself renders identically to the plain
// fmt.Errorf every provider produced before this type existed, so
// introducing it changes no persisted string (runs.error, surfaced by
// `runs get`, the API trace, and pkg/agentforge's Run.Error) and requires
// no test update.
type Error struct {
	// Provider is the short name used in Error()'s prefix: "anthropic",
	// "openai", "gemini", or "ollama".
	Provider string
	// StatusCode is the HTTP status a provider returned, or 0 if the
	// request never got a response at all (a transport failure).
	StatusCode int
	// Message is the provider-extracted human-readable error message
	// (each provider has its own errMsg func for pulling this out of an
	// error response body).
	Message string
	// RetryAfter is how long the provider asked callers to wait before
	// retrying, parsed from a Retry-After header (Anthropic, OpenAI) or,
	// for providers that don't send one, an equivalent field in the error
	// body (Gemini's google.rpc.RetryInfo). Zero means none was
	// advertised, not "retry immediately".
	RetryAfter time.Duration
	// Err is the wrapped transport error. Set only when StatusCode == 0.
	Err error
}

// Error renders identically to the two fmt.Errorf shapes every provider
// used before this type existed — see the two doRequest/decodeResponse
// call sites this replaces in anthropic.go, openai.go, gemini.go, and
// ollama.go.
func (e *Error) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("%s: status %d: %s", e.Provider, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("%s: request failed: %v", e.Provider, e.Err)
}

// Unwrap lets errors.Is/As reach the transport error a Complete/Stream
// call wrapped (net.Error, context.DeadlineExceeded, ...).
func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether this failure is likely transient and worth
// retrying: a rate limit, a momentary server-side or gateway error, or a
// transport failure that never reached an HTTP response at all. Anything
// else (400/401/403/404/413/422, ...) is a config or prompt problem that
// will fail identically forever — retrying it would just delay a failure
// that should surface immediately.
func (e *Error) Retryable() bool {
	if e.StatusCode == 0 {
		return true // transport failure: connection reset, timeout, DNS, ...
	}
	switch e.StatusCode {
	case http.StatusRequestTimeout, // 408
		http.StatusTooEarly,            // 425
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
		529:                            // Anthropic's overloaded_error — the single most common Anthropic outage
		return true
	}
	return false
}

// newStatusError builds an *Error from a non-2xx HTTP response whose body
// has already been read into respBody. msg is the provider-specific
// human-readable message extracted from that body. retryAfterFromBody, if
// non-nil, lets a provider with no Retry-After header (Gemini puts the
// delay in the error body instead) supply an equivalent extractor; it's
// only consulted when the header itself was absent.
func newStatusError(providerName string, resp *http.Response, respBody []byte, msg string, retryAfterFromBody func([]byte) time.Duration) *Error {
	e := &Error{
		Provider:   providerName,
		StatusCode: resp.StatusCode,
		Message:    msg,
		RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
	}
	if e.RetryAfter == 0 && retryAfterFromBody != nil {
		e.RetryAfter = retryAfterFromBody(respBody)
	}
	return e
}

// newTransportError builds an *Error for a failure that happened before
// any HTTP response was received.
func newTransportError(providerName string, err error) *Error {
	return &Error{Provider: providerName, Err: err}
}

// parseRetryAfter parses a Retry-After header value in either RFC 9110
// form: delta-seconds ("120") or an HTTP-date
// ("Wed, 21 Oct 2015 07:28:00 GMT"). Returns 0 — "none advertised", not
// "retry immediately" — for an empty, negative, or unparseable value; the
// retry policy's own backoff takes over in that case.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
