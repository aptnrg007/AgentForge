package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// Mirrors ollama_format_test.go's shape (present-when-set / absent-when-zero,
// across both Complete and Stream) for the same reason that file gives:
// NumCtx is another Ollama-specific option threaded through options, not the
// top-level request body, so it needs its own presence/absence pinning
// independent of ResponseSchema's.

func TestOllamaCompleteSendsNumCtxWhenSet(t *testing.T) {
	var captured map[string]any
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Done: true})
	})

	if _, err := o.Complete(context.Background(), Request{Model: "m", NumCtx: 16384}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	options, ok := captured["options"].(map[string]any)
	if !ok {
		t.Fatal("expected an options object on the request")
	}
	numCtx, ok := options["num_ctx"]
	if !ok {
		t.Fatal("expected options.num_ctx to be set")
	}
	if numCtx != float64(16384) {
		t.Fatalf("options.num_ctx = %v, want 16384", numCtx)
	}
}

func TestOllamaCompleteOmitsNumCtxWhenZero(t *testing.T) {
	var captured map[string]any
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Done: true})
	})

	if _, err := o.Complete(context.Background(), Request{Model: "m"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if options, ok := captured["options"].(map[string]any); ok {
		if _, ok := options["num_ctx"]; ok {
			t.Fatalf("expected no num_ctx field when NumCtx is zero, got %v", options["num_ctx"])
		}
	}
}

func TestOllamaStreamSendsNumCtxWhenSet(t *testing.T) {
	var captured map[string]any
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		fl := w.(http.Flusher)
		writeChunk(t, w, fl, ollamaChatResponse{Done: true})
	})

	stream, err := o.Stream(context.Background(), Request{Model: "m", NumCtx: 24576})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	drainStream(t, stream)

	options, ok := captured["options"].(map[string]any)
	if !ok {
		t.Fatal("expected an options object on the streaming request")
	}
	if numCtx := options["num_ctx"]; numCtx != float64(24576) {
		t.Fatalf("options.num_ctx = %v, want 24576", numCtx)
	}
}
