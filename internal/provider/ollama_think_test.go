package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// Mirrors ollama_numctx_test.go's shape, but Think is a top-level request
// field (confirmed live against Ollama), not one nested under "options" —
// so these assert against the top level of the captured request instead.

func boolPtr(b bool) *bool { return &b }

func TestOllamaCompleteSendsThinkFalseWhenSet(t *testing.T) {
	var captured map[string]any
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Done: true})
	})

	if _, err := o.Complete(context.Background(), Request{Model: "m", Think: boolPtr(false)}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	think, ok := captured["think"]
	if !ok {
		t.Fatal("expected top-level think field to be set")
	}
	if think != false {
		t.Fatalf("think = %v, want false", think)
	}
}

func TestOllamaCompleteOmitsThinkWhenNil(t *testing.T) {
	var captured map[string]any
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Done: true})
	})

	if _, err := o.Complete(context.Background(), Request{Model: "m"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := captured["think"]; ok {
		t.Fatalf("expected no think field when Think is nil, got %v", captured["think"])
	}
}

func TestOllamaStreamSendsThinkFalseWhenSet(t *testing.T) {
	var captured map[string]any
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		fl := w.(http.Flusher)
		writeChunk(t, w, fl, ollamaChatResponse{Done: true})
	})

	stream, err := o.Stream(context.Background(), Request{Model: "m", Think: boolPtr(false)})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	drainStream(t, stream)

	if think := captured["think"]; think != false {
		t.Fatalf("think = %v, want false", think)
	}
}
