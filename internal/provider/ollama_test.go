package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agentforge/internal/message"
)

// newTestOllama points an Ollama provider at an httptest.Server running
// handler, and registers cleanup.
func newTestOllama(t *testing.T, handler http.HandlerFunc) *Ollama {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewOllama(srv.URL)
}

// writeChunk marshals v as one NDJSON line and flushes it immediately, so
// the client can observe it before the handler writes anything more.
func writeChunk(t *testing.T, w http.ResponseWriter, fl http.Flusher, v ollamaChatResponse) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	if _, err := fmt.Fprintf(w, "%s\n", data); err != nil {
		t.Fatalf("write chunk: %v", err)
	}
	fl.Flush()
}

func drainStream(t *testing.T, s Stream) (deltas []string) {
	t.Helper()
	for s.Next() {
		deltas = append(deltas, s.Delta())
	}
	return deltas
}

func TestStreamAssemblesTextAcrossChunks(t *testing.T) {
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeChunk(t, w, fl, ollamaChatResponse{Message: ollamaMessage{Content: "Hel"}})
		writeChunk(t, w, fl, ollamaChatResponse{Message: ollamaMessage{Content: "lo, "}})
		writeChunk(t, w, fl, ollamaChatResponse{Message: ollamaMessage{Content: "world"}, Done: true, DoneReason: "stop"})
	})

	stream, err := o.Stream(context.Background(), Request{Model: "test"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	deltas := drainStream(t, stream)
	if err := stream.Err(); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}
	if got := fmt.Sprint(deltas); got != `[Hel lo,  world]` {
		t.Fatalf("expected three deltas Hel/lo, /world, got %v", deltas)
	}

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != message.BlockText || resp.Content[0].Text != "Hello, world" {
		t.Fatalf("expected a single assembled text block \"Hello, world\", got %+v", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("expected end_turn, got %q", resp.StopReason)
	}
}

func TestStreamAssemblesToolCallsMidStream(t *testing.T) {
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeChunk(t, w, fl, ollamaChatResponse{Message: ollamaMessage{Content: "checking..."}})
		writeChunk(t, w, fl, ollamaChatResponse{Message: ollamaMessage{
			ToolCalls: []ollamaToolCall{{Function: ollamaFunctionCall{Name: "sum", Arguments: map[string]any{"a": float64(1), "b": float64(2)}}}},
		}})
		writeChunk(t, w, fl, ollamaChatResponse{Done: true, DoneReason: "stop"})
	})

	stream, err := o.Stream(context.Background(), Request{Model: "test"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	drainStream(t, stream)
	if err := stream.Err(); err != nil {
		t.Fatalf("unexpected stream error: %v", err)
	}

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("expected a text block and a tool_use block, got %+v", resp.Content)
	}
	if resp.Content[0].Type != message.BlockText || resp.Content[0].Text != "checking..." {
		t.Fatalf("expected leading text block, got %+v", resp.Content[0])
	}
	tu := resp.Content[1]
	if tu.Type != message.BlockToolUse || tu.Name != "sum" {
		t.Fatalf("expected a sum tool_use block, got %+v", tu)
	}
	if tu.ID == "" {
		t.Fatal("expected a generated tool call ID")
	}
	var args map[string]any
	if err := json.Unmarshal(tu.Input, &args); err != nil {
		t.Fatalf("unmarshal tool args: %v", err)
	}
	if args["a"] != float64(1) || args["b"] != float64(2) {
		t.Fatalf("expected args a=1 b=2, got %v", args)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("expected tool_use, got %q", resp.StopReason)
	}
}

func TestStreamMalformedChunkSurfacesErr(t *testing.T) {
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeChunk(t, w, fl, ollamaChatResponse{Message: ollamaMessage{Content: "ok so far"}})
		fmt.Fprint(w, "{not valid json\n")
		fl.Flush()
	})

	stream, err := o.Stream(context.Background(), Request{Model: "test"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	drainStream(t, stream)
	if stream.Err() == nil {
		t.Fatal("expected an error after a malformed chunk")
	}
	if _, err := stream.Response(); err == nil {
		t.Fatal("expected Response to surface the same error")
	}
}

func TestStreamContextCancellationStopsStream(t *testing.T) {
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeChunk(t, w, fl, ollamaChatResponse{Message: ollamaMessage{Content: "first"}})
		// Block until the client goes away instead of ever completing, to
		// simulate a long-running generation the caller cancels out of.
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := o.Stream(ctx, Request{Model: "test"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	if !stream.Next() {
		t.Fatalf("expected the first chunk before cancellation, err=%v", stream.Err())
	}

	cancel()

	done := make(chan bool, 1)
	go func() { done <- stream.Next() }()
	select {
	case more := <-done:
		if more {
			t.Fatal("expected Next to return false after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Next did not return after context cancellation")
	}
	if stream.Err() == nil {
		t.Fatal("expected a context-cancellation error")
	}
}

// TestStreamTokensArriveIncrementally is the phase's headline acceptance
// test: it proves Stream delivers a delta before the response is complete,
// not merely after buffering the whole body. If Stream (or ollamaStream)
// ever regressed to reading the full body up front, this test would hang
// until the release channel closes at the very end and then fail deltas
// ordering — instead it must observe the first delta *before* signaling
// the server to send the rest.
func TestStreamTokensArriveIncrementally(t *testing.T) {
	release := make(chan struct{})
	o := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeChunk(t, w, fl, ollamaChatResponse{Message: ollamaMessage{Content: "first "}})
		<-release // held open until the test explicitly releases it
		writeChunk(t, w, fl, ollamaChatResponse{Message: ollamaMessage{Content: "second"}, Done: true, DoneReason: "stop"})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := o.Stream(ctx, Request{Model: "test"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	if !stream.Next() {
		t.Fatalf("expected a first chunk, err=%v", stream.Err())
	}
	if stream.Delta() != "first " {
		t.Fatalf("expected first delta %q, got %q", "first ", stream.Delta())
	}

	// Only now unblock the server. If we got here, the client demonstrably
	// did not wait for the whole body before surfacing the first delta.
	close(release)

	if !stream.Next() {
		t.Fatalf("expected a second chunk, err=%v", stream.Err())
	}
	if stream.Delta() != "second" {
		t.Fatalf("expected second delta %q, got %q", "second", stream.Delta())
	}
	if stream.Next() {
		t.Fatal("expected the stream to end after the done chunk")
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	resp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "first second" {
		t.Fatalf("expected assembled text %q, got %+v", "first second", resp.Content)
	}
}

// TestCompleteAndStreamProduceEquivalentResponses checks that Complete
// (single JSON body) and Stream (NDJSON chunks) assemble structurally
// identical responses for equivalent wire content — proving the two,
// deliberately independent, implementations agree.
func TestCompleteAndStreamProduceEquivalentResponses(t *testing.T) {
	nonStreaming := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		resp := ollamaChatResponse{
			Message: ollamaMessage{
				Content:   "the sum is 3",
				ToolCalls: []ollamaToolCall{{Function: ollamaFunctionCall{Name: "sum", Arguments: map[string]any{"a": float64(1), "b": float64(2)}}}},
			},
			Done: true, DoneReason: "stop",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	streaming := newTestOllama(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		writeChunk(t, w, fl, ollamaChatResponse{Message: ollamaMessage{Content: "the sum is 3"}})
		writeChunk(t, w, fl, ollamaChatResponse{
			Message: ollamaMessage{ToolCalls: []ollamaToolCall{{Function: ollamaFunctionCall{Name: "sum", Arguments: map[string]any{"a": float64(1), "b": float64(2)}}}}},
			Done:    true, DoneReason: "stop",
		})
	})

	completeResp, err := nonStreaming.Complete(context.Background(), Request{Model: "test"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	stream, err := streaming.Stream(context.Background(), Request{Model: "test"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	drainStream(t, stream)
	streamResp, err := stream.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}

	if completeResp.StopReason != streamResp.StopReason {
		t.Fatalf("stop reasons differ: Complete=%q Stream=%q", completeResp.StopReason, streamResp.StopReason)
	}
	if len(completeResp.Content) != len(streamResp.Content) {
		t.Fatalf("block counts differ: Complete=%d Stream=%d", len(completeResp.Content), len(streamResp.Content))
	}
	for i := range completeResp.Content {
		c, s := completeResp.Content[i], streamResp.Content[i]
		// IDs are independently generated random tool-call IDs on each
		// call, so compare everything else.
		if c.Type != s.Type || c.Text != s.Text || c.Name != s.Name || string(c.Input) != string(s.Input) {
			t.Fatalf("block %d differs: Complete=%+v Stream=%+v", i, c, s)
		}
	}
}
