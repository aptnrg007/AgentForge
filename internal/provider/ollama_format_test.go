package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestOllamaAdvertisesStructuredOutputCapability(t *testing.T) {
	o := NewOllama("")
	caps := o.Capabilities()
	if !caps.StructuredOutput {
		t.Fatal("expected Ollama to advertise StructuredOutput support")
	}
	if !caps.ToolUse {
		t.Fatal("expected Ollama to still advertise ToolUse support")
	}
}

func TestOllamaCompleteSendsFormatWhenResponseSchemaSet(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	var captured map[string]any
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Done: true})
	})

	if _, err := o.Complete(context.Background(), Request{Model: "m", ResponseSchema: schema}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	got, ok := captured["format"]
	if !ok {
		t.Fatal("expected a format field on the request")
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal captured format: %v", err)
	}
	if string(gotJSON) != string(schema) {
		t.Fatalf("format = %s, want %s", gotJSON, schema)
	}
}

func TestOllamaCompleteOmitsFormatWhenNoResponseSchema(t *testing.T) {
	var captured map[string]any
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ollamaChatResponse{Done: true})
	})

	if _, err := o.Complete(context.Background(), Request{Model: "m"}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if _, ok := captured["format"]; ok {
		t.Fatalf("expected no format field on the request when ResponseSchema is unset, got %v", captured["format"])
	}
}

func TestOllamaStreamSendsFormatWhenResponseSchemaSet(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	var captured map[string]any
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		fl := w.(http.Flusher)
		writeChunk(t, w, fl, ollamaChatResponse{Done: true})
	})

	stream, err := o.Stream(context.Background(), Request{Model: "m", ResponseSchema: schema})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	drainStream(t, stream)

	got, ok := captured["format"]
	if !ok {
		t.Fatal("expected a format field on the streaming request")
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal captured format: %v", err)
	}
	if string(gotJSON) != string(schema) {
		t.Fatalf("format = %s, want %s", gotJSON, schema)
	}
}
