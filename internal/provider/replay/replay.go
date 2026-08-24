// Package replay implements a provider.Provider that replays a scripted
// sequence of previously-recorded responses instead of calling a live
// model — the fixture-replay approach docs/DESIGN.md section 11 describes,
// promoted out of internal/runtime's test-local fakeProvider so
// internal/eval can use the same shape outside of _test.go files.
//
// Provider is the replay half: it satisfies provider.Provider by handing
// back Fixture.Turns in order. Recorder is the record half: it wraps a
// live provider.Provider, forwards every call through unchanged, and
// accumulates a Fixture that Save writes to disk — the output Provider
// then replays.
package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"agentforge/internal/provider"
)

// Turn is one recorded model call. A struct (not a bare *provider.Response
// slice element) so the fixture format can grow a field — a request
// summary, timing — without an incompatible schema change.
type Turn struct {
	Response *provider.Response `json:"response"`
}

// Fixture is the on-disk shape written by Recorder.Save and read by Load:
// one scripted model call per Turn, consumed in order.
type Fixture struct {
	Turns []Turn `json:"turns"`
}

// Provider replays a Fixture's turns in order, one per Complete or Stream
// call, and satisfies provider.Provider. Calling it more times than it has
// turns is a fixture/case mismatch, not a live-model failure, so it
// returns a distinct, greppable error instead of a generic one.
type Provider struct {
	fixture Fixture
	calls   int
	caps    provider.Capabilities
}

// New builds a Provider directly from responses, without going through a
// fixture file — useful for constructing one in code (e.g. a case with no
// recorded fixture yet, or a synthetic one).
func New(responses []*provider.Response, caps provider.Capabilities) *Provider {
	turns := make([]Turn, len(responses))
	for i, r := range responses {
		turns[i] = Turn{Response: r}
	}
	return &Provider{fixture: Fixture{Turns: turns}, caps: caps}
}

// Load reads a Fixture from path and returns a Provider that replays it.
func Load(path string, caps provider.Capabilities) (*Provider, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("replay: load fixture %s: %w", path, err)
	}
	var fx Fixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		return nil, fmt.Errorf("replay: parse fixture %s: %w", path, err)
	}
	return &Provider{fixture: fx, caps: caps}, nil
}

func (p *Provider) Name() string { return "replay" }

func (p *Provider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	if p.calls >= len(p.fixture.Turns) {
		return nil, fmt.Errorf("replay: fixture has %d turn(s), but the run made a %d%s call — the recorded fixture doesn't match this case anymore, re-record it", len(p.fixture.Turns), p.calls+1, ordinalSuffix(p.calls+1))
	}
	resp := p.fixture.Turns[p.calls].Response
	p.calls++
	return resp, nil
}

// Stream re-plays the same recorded response as a single-chunk stream —
// there is no way to replay real token-by-token timing from a Fixture,
// and the engine only calls Stream when an event sink is installed (see
// runtime.Engine.complete), which the eval harness never does.
func (p *Provider) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	resp, err := p.Complete(ctx, r)
	if err != nil {
		return nil, err
	}
	return provider.NewResponseStream(resp), nil
}

func (p *Provider) Capabilities() provider.Capabilities { return p.caps }

func ordinalSuffix(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return "th"
	}
	switch n % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	default:
		return "th"
	}
}

// Recorder wraps a live provider.Provider, forwarding every Complete/
// Stream call to it unchanged, while accumulating each response into a
// Fixture. Save then writes that Fixture to disk for a later Provider to
// replay. Safe for concurrent use — nothing in this codebase drives two
// model calls for the same run concurrently, but a suite may run several
// cases against the same Recorder-wrapped agent build.
type Recorder struct {
	inner provider.Provider

	mu      sync.Mutex
	fixture Fixture
}

func NewRecorder(inner provider.Provider) *Recorder {
	return &Recorder{inner: inner}
}

func (r *Recorder) Name() string { return r.inner.Name() }

func (r *Recorder) Complete(ctx context.Context, req provider.Request) (*provider.Response, error) {
	resp, err := r.inner.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	r.record(resp)
	return resp, nil
}

// Stream records the fully-assembled response after the wrapped
// provider's stream finishes, then hands the caller a fresh stream over
// that same response — a Recorder-wrapped provider is transparent to a
// caller that installs an event sink, it just also captures the result.
func (r *Recorder) Stream(ctx context.Context, req provider.Request) (provider.Stream, error) {
	stream, err := r.inner.Stream(ctx, req)
	if err != nil {
		return nil, err
	}
	for stream.Next() {
	}
	if err := stream.Err(); err != nil {
		stream.Close()
		return nil, err
	}
	resp, err := stream.Response()
	stream.Close()
	if err != nil {
		return nil, err
	}
	r.record(resp)
	return provider.NewResponseStream(resp), nil
}

func (r *Recorder) Capabilities() provider.Capabilities { return r.inner.Capabilities() }

func (r *Recorder) record(resp *provider.Response) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fixture.Turns = append(r.fixture.Turns, Turn{Response: resp})
}

// Save writes the Fixture accumulated so far to path as indented JSON,
// creating parent directories as needed.
func (r *Recorder) Save(path string) error {
	r.mu.Lock()
	fx := r.fixture
	r.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("replay: save fixture %s: %w", path, err)
	}
	raw, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		return fmt.Errorf("replay: save fixture %s: %w", path, err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("replay: save fixture %s: %w", path, err)
	}
	return nil
}
