package replay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentforge/internal/message"
	"agentforge/internal/provider"
)

func textResponse(text string) *provider.Response {
	return &provider.Response{
		Content:    []message.ContentBlock{{Type: message.BlockText, Text: text}},
		StopReason: "end_turn",
	}
}

func TestProviderReplaysInOrder(t *testing.T) {
	p := New([]*provider.Response{textResponse("one"), textResponse("two")}, provider.Capabilities{})

	r1, err := p.Complete(context.Background(), provider.Request{})
	if err != nil {
		t.Fatalf("Complete #1: %v", err)
	}
	if r1.Content[0].Text != "one" {
		t.Fatalf("Complete #1 = %q, want %q", r1.Content[0].Text, "one")
	}

	r2, err := p.Complete(context.Background(), provider.Request{})
	if err != nil {
		t.Fatalf("Complete #2: %v", err)
	}
	if r2.Content[0].Text != "two" {
		t.Fatalf("Complete #2 = %q, want %q", r2.Content[0].Text, "two")
	}
}

func TestProviderErrorsWhenFixtureExhausted(t *testing.T) {
	p := New([]*provider.Response{textResponse("one")}, provider.Capabilities{})

	if _, err := p.Complete(context.Background(), provider.Request{}); err != nil {
		t.Fatalf("Complete #1: %v", err)
	}
	_, err := p.Complete(context.Background(), provider.Request{})
	if err == nil || !strings.Contains(err.Error(), "doesn't match this case anymore") {
		t.Fatalf("Complete #2: got %v, want a fixture-exhausted error", err)
	}
}

func TestProviderStreamReplaysAsSingleChunk(t *testing.T) {
	p := New([]*provider.Response{textResponse("hello")}, provider.Capabilities{})

	stream, err := p.Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var got strings.Builder
	for stream.Next() {
		got.WriteString(stream.Delta())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream.Err: %v", err)
	}
	if got.String() != "hello" {
		t.Fatalf("streamed text = %q, want %q", got.String(), "hello")
	}
}

func TestLoadReadsWhatSaveWrote(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "fixture.json")

	rec := NewRecorder(&fakeLive{resp: textResponse("recorded")})
	if _, err := rec.Complete(context.Background(), provider.Request{}); err != nil {
		t.Fatalf("Recorder.Complete: %v", err)
	}
	if err := rec.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected Save to create %s: %v", path, err)
	}

	p, err := Load(path, provider.Capabilities{})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	resp, err := p.Complete(context.Background(), provider.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content[0].Text != "recorded" {
		t.Fatalf("replayed text = %q, want %q", resp.Content[0].Text, "recorded")
	}
}

func TestRecorderForwardsToInnerProvider(t *testing.T) {
	inner := &fakeLive{resp: textResponse("live answer")}
	rec := NewRecorder(inner)

	resp, err := rec.Complete(context.Background(), provider.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content[0].Text != "live answer" {
		t.Fatalf("Complete = %q, want the inner provider's response", resp.Content[0].Text)
	}
	if inner.calls != 1 {
		t.Fatalf("inner.calls = %d, want 1", inner.calls)
	}
}

// fakeLive is a minimal live-provider stand-in for Recorder tests.
type fakeLive struct {
	resp  *provider.Response
	calls int
}

func (f *fakeLive) Name() string { return "fake-live" }

func (f *fakeLive) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	f.calls++
	return f.resp, nil
}

func (f *fakeLive) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	resp, err := f.Complete(ctx, r)
	if err != nil {
		return nil, err
	}
	return provider.NewResponseStream(resp), nil
}

func (f *fakeLive) Capabilities() provider.Capabilities { return provider.Capabilities{} }
