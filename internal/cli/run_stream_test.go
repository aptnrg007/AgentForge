package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These test runRemote's SSE client against a hand-rolled fake daemon
// (not a real internal/api.Server — that always builds its provider via
// agent.DefaultProviderFactory, with no way to inject a scripted one from
// outside the api package) — exercising exactly what's new here: parsing
// "event:"/"data:" frames off a real HTTP connection, and the follow-up
// GET /v1/runs/{id} that fills in the message history the stream itself
// doesn't carry.

const runStreamTestYAML = `
name: minimal
model:
  provider: ollama
  name: test-model
instructions: you are a test assistant
`

// writeTestConfig writes yaml to a temp file and returns its path.
func writeTestConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// newFakeDaemon wires up a fake POST /v1/agents, POST
// /v1/agents/{name}/stream (writing sseBody verbatim, flushing after each
// write so a real client sees it as it arrives rather than buffered), and
// GET /v1/runs/{id} (serving traceJSON) — the three endpoints runRemote's
// streaming path calls.
func newFakeDaemon(t *testing.T, agentName, runID, sseBody, traceJSON string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"name":%q,"yaml":"","created_at":0,"updated_at":0}`, agentName)
	})
	mux.HandleFunc("POST /v1/agents/"+agentName+"/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("httptest ResponseWriter does not support flushing")
		}
		for _, chunk := range strings.SplitAfter(sseBody, "\n\n") {
			if chunk == "" {
				continue
			}
			fmt.Fprint(w, chunk)
			fl.Flush()
		}
	})
	mux.HandleFunc("GET /v1/runs/"+runID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, traceJSON)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestRunRemoteStreamsThenFetchesFinalTrace(t *testing.T) {
	const runID = "run_streamed_1"
	sse := "event: token\ndata: {\"text\":\"Hel\"}\n\n" +
		"event: token\ndata: {\"text\":\"lo\"}\n\n" +
		fmt.Sprintf("event: done\ndata: {\"run_id\":%q,\"state\":\"completed\"}\n\n", runID)
	trace := fmt.Sprintf(`{
		"run_id": %q, "state": "completed", "turn_count": 1,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "hi"}]},
			{"role": "assistant", "content": [{"type": "text", "text": "Hello"}]}
		],
		"tool_calls": []
	}`, runID)

	ts := newFakeDaemon(t, "minimal", runID, sse, trace)
	cfgPath := writeTestConfig(t, runStreamTestYAML)
	outPath := filepath.Join(t.TempDir(), "out.json")

	if err := runRemote(context.Background(), ts.URL, "", cfgPath, "hi", outputOptions{format: "json", path: outPath}); err != nil {
		t.Fatalf("runRemote: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read --output file: %v", err)
	}
	var env struct {
		RunID          string          `json:"run_id"`
		State          string          `json:"state"`
		ToolCallsCount int             `json:"tool_calls_count"`
		Output         json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, data)
	}
	if env.RunID != runID {
		t.Fatalf("run_id = %q, want %q", env.RunID, runID)
	}
	if env.State != "completed" {
		t.Fatalf("state = %q, want completed", env.State)
	}
	if string(env.Output) != `"Hello"` {
		t.Fatalf("output = %s, want \"Hello\" (the final trace's assistant text)", env.Output)
	}
}

func TestRunRemoteAwaitingApprovalReportsPendingWithoutTrace(t *testing.T) {
	const runID = "run_streamed_pending"
	sse := "event: tool_call\ndata: {\"call_id\":\"call_1\",\"tool\":\"danger.tool\",\"args\":{\"x\":1}}\n\n" +
		fmt.Sprintf("event: awaiting_approval\ndata: {\"run_id\":%q,\"pending\":[{\"call_id\":\"call_1\",\"tool\":\"danger.tool\",\"args\":{\"x\":1}}]}\n\n", runID)
	trace := fmt.Sprintf(`{
		"run_id": %q, "state": "awaiting_approval", "turn_count": 1,
		"messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}],
		"tool_calls": [{"id":"call_1","tool_name":"danger.tool","args":{"x":1},"approval":"pending","created_at":0}]
	}`, runID)

	ts := newFakeDaemon(t, "minimal", runID, sse, trace)
	cfgPath := writeTestConfig(t, runStreamTestYAML)
	outPath := filepath.Join(t.TempDir(), "out.json")

	if err := runRemote(context.Background(), ts.URL, "", cfgPath, "hi", outputOptions{format: "json", path: outPath}); err != nil {
		t.Fatalf("runRemote: %v", err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read --output file: %v", err)
	}
	var env struct {
		State   string `json:"state"`
		Pending []struct {
			CallID string `json:"call_id"`
			Tool   string `json:"tool"`
		} `json:"pending"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, data)
	}
	if env.State != "awaiting_approval" {
		t.Fatalf("state = %q, want awaiting_approval", env.State)
	}
	if len(env.Pending) != 1 || env.Pending[0].CallID != "call_1" || env.Pending[0].Tool != "danger.tool" {
		t.Fatalf("expected one pending call_1/danger.tool, got %+v", env.Pending)
	}
}

func TestReadSSEParsesMultipleFrames(t *testing.T) {
	body := "event: token\ndata: {\"text\":\"a\"}\n\n" +
		"event: token\ndata: {\"text\":\"b\"}\n\n" +
		"event: done\ndata: {\"run_id\":\"r1\",\"state\":\"completed\"}\n\n"

	var got []sseEvent
	if err := readSSE(bytes.NewReader([]byte(body)), func(ev sseEvent) bool {
		got = append(got, ev)
		return ev.Event == "done"
	}); err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 frames, got %d: %+v", len(got), got)
	}
	if got[0].Event != "token" || string(got[0].Data) != `{"text":"a"}` {
		t.Fatalf("frame 0 = %+v", got[0])
	}
	if got[2].Event != "done" || string(got[2].Data) != `{"run_id":"r1","state":"completed"}` {
		t.Fatalf("frame 2 = %+v", got[2])
	}
}

// TestReadSSEStopsEarlyWithoutDrainingTrailingFrames confirms fn's stop
// signal actually short-circuits — a "done" mid-body must not make
// readSSE report a spurious frame after it.
func TestReadSSEStopsEarlyWithoutDrainingTrailingFrames(t *testing.T) {
	body := "event: done\ndata: {\"run_id\":\"r1\",\"state\":\"completed\"}\n\n" +
		"event: token\ndata: {\"text\":\"should never be read\"}\n\n"

	var got []sseEvent
	if err := readSSE(bytes.NewReader([]byte(body)), func(ev sseEvent) bool {
		got = append(got, ev)
		return true
	}); err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 frame (stop honored), got %d: %+v", len(got), got)
	}
}
