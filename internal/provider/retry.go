package provider

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"
)

// RetryPolicy configures WithRetry. MaxAttempts <= 1 turns the decorator
// into a strict passthrough — no wrapping, no logging, byte-identical to
// having no decorator at all — which is what makes an agent config with
// no retry: block (or retry.max_attempts: 1) provably behave exactly as
// it did before this package existed.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts, including the first.
	MaxAttempts int
	// InitialDelay is the backoff before the first retry.
	InitialDelay time.Duration
	// MaxDelay caps a single backoff sleep (before jitter).
	MaxDelay time.Duration
	// MaxElapsed caps the total time spent retrying one call, across
	// every attempt. 0 means unbounded (still implicitly bounded by
	// ctx's own deadline, if it has one).
	MaxElapsed time.Duration
	// OnNetworkError also retries a transport failure (StatusCode == 0 —
	// a dropped connection, DNS failure, ...) that never reached an HTTP
	// response, not just a retryable HTTP status.
	OnNetworkError bool

	// sleep is a test seam: nil means the real ctx-aware sleep. Tests
	// inject a fake that returns immediately so backoff delays don't
	// actually elapse.
	sleep func(context.Context, time.Duration) error
}

// RetriesExhaustedError is returned when a call's retry budget (attempts,
// MaxElapsed, or ctx's own deadline) runs out before it succeeded. It
// wraps the last error the call produced, so errors.As still reaches the
// underlying *Error and the status it named.
type RetriesExhaustedError struct {
	Attempts int
	Elapsed  time.Duration
	Err      error
}

func (e *RetriesExhaustedError) Error() string {
	return fmt.Sprintf("%v (after %d attempts over %s)", e.Err, e.Attempts, e.Elapsed.Round(time.Millisecond))
}

func (e *RetriesExhaustedError) Unwrap() error { return e.Err }

// WithRetry wraps p so a retryable failure (see (*Error).Retryable) is
// retried with backoff before being returned to the caller, instead of
// ending the run on the first transient error. logger receives one
// warning line per retry; nil defaults to slog.Default(), matching
// runtime.Config.Logger's convention.
//
// Retrying is safe here specifically because a provider call is the only
// externally-observable action in the run loop with no side effects —
// tools only execute after a separate persisted ready_for_tools
// transition (internal/runtime), so a duplicate model call can never
// double-execute a tool. The only cost of a retry is tokens.
func WithRetry(p Provider, pol RetryPolicy, logger *slog.Logger) Provider {
	if logger == nil {
		logger = slog.Default()
	}
	return &retryProvider{inner: p, policy: pol, logger: logger}
}

type retryProvider struct {
	inner  Provider
	policy RetryPolicy
	logger *slog.Logger
}

func (r *retryProvider) Name() string               { return r.inner.Name() }
func (r *retryProvider) Capabilities() Capabilities { return r.inner.Capabilities() }

func (r *retryProvider) Complete(ctx context.Context, req Request) (*Response, error) {
	return retryCall(ctx, r.policy, r.logger, r.inner.Name(), func() (*Response, error) {
		return r.inner.Complete(ctx, req)
	})
}

// Stream retries establishing the stream — the Stream() call itself —
// exactly like Complete. Once a non-nil Stream has been handed back to
// the caller, this decorator is out of the picture: a failure surfaced
// later through Stream.Err() is never retried, because by then tokens
// may already have reached Engine.emit, and replaying them would show a
// caller the same output twice. This is safe by construction rather than
// by discipline: every provider's Stream checks resp.StatusCode before
// constructing a Stream at all, so a retryable failure is always caught
// here, pre-first-token, never mid-stream.
func (r *retryProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return retryCall(ctx, r.policy, r.logger, r.inner.Name(), func() (Stream, error) {
		return r.inner.Stream(ctx, req)
	})
}

// retryCall runs call, retrying a retryable *Error per pol until it
// succeeds, a non-retryable error is returned, or the budget (attempts,
// MaxElapsed, or ctx's own deadline) is spent.
func retryCall[T any](ctx context.Context, pol RetryPolicy, logger *slog.Logger, providerName string, call func() (T, error)) (T, error) {
	attempts := pol.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}
	if attempts <= 1 {
		return call()
	}

	sleep := pol.sleep
	if sleep == nil {
		sleep = ctxSleep
	}

	start := time.Now()
	for attempt := 1; ; attempt++ {
		result, err := call()
		if err == nil {
			return result, nil
		}

		var perr *Error
		retryable := errors.As(err, &perr) && perr.Retryable()
		if retryable && perr.StatusCode == 0 && !pol.OnNetworkError {
			retryable = false
		}
		if !retryable {
			return result, err
		}
		if attempt >= attempts {
			var zero T
			return zero, &RetriesExhaustedError{Attempts: attempt, Elapsed: time.Since(start), Err: err}
		}

		delay := backoffDelay(pol, attempt, perr)
		elapsed := time.Since(start)
		if pol.MaxElapsed > 0 && elapsed+delay > pol.MaxElapsed {
			var zero T
			return zero, &RetriesExhaustedError{Attempts: attempt, Elapsed: elapsed, Err: err}
		}
		// Stop before sleeping into the run's own deadline rather than
		// letting it expire mid-sleep: past this point ctx.Err() would
		// become DeadlineExceeded and stepModel's classifier would
		// report "run exceeded its time limit", hiding the retryable
		// error that actually caused it.
		if dl, ok := ctx.Deadline(); ok && time.Now().Add(delay).After(dl) {
			var zero T
			return zero, &RetriesExhaustedError{Attempts: attempt, Elapsed: elapsed, Err: err}
		}

		logger.Warn("provider: retrying after a transient error",
			"provider", providerName, "attempt", attempt, "max_attempts", attempts,
			"delay", delay, "error", err)

		if serr := sleep(ctx, delay); serr != nil {
			// ctx died during backoff — an external cancel or the run's
			// own deadline. Return the last provider error, not ctx.Err():
			// stepModel deliberately classifies on ctx.Err() rather than
			// the returned error, so this keeps the useful status text in
			// runs.error while the existing classifier still picks the
			// right terminal state.
			return result, err
		}
	}
}

// backoffDelay computes the wait before the next attempt: perr's
// Retry-After when the provider advertised one (verbatim, no jitter — the
// server named a time; honor it, unless it's implausibly large), else
// exponential backoff from pol.InitialDelay with equal jitter (half fixed,
// half random) so the first retry isn't near-instant against a server
// that just said 429.
func backoffDelay(pol RetryPolicy, attempt int, perr *Error) time.Duration {
	maxDelay := pol.MaxDelay
	if maxDelay <= 0 {
		maxDelay = 30 * time.Second
	}

	if perr != nil && perr.RetryAfter > 0 {
		// A hostile or buggy Retry-After (e.g. "3600") must not park a
		// run for an hour; fall through to computed backoff instead.
		if perr.RetryAfter <= maxDelay*4 {
			return perr.RetryAfter
		}
	}

	initialDelay := pol.InitialDelay
	if initialDelay <= 0 {
		initialDelay = time.Second
	}
	shift := attempt - 1
	if shift > 30 { // guard against overflowing the bit shift
		shift = 30
	}
	d := initialDelay * time.Duration(1<<uint(shift))
	if d <= 0 || d > maxDelay { // d <= 0 covers the overflow case directly
		d = maxDelay
	}

	half := d / 2
	if half <= 0 {
		return d
	}
	return half + time.Duration(rand.Int64N(int64(half)))
}

// ctxSleep waits for d, or returns ctx.Err() early if ctx is done first.
func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
