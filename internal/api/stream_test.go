package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"agentforge/internal/config"
	"agentforge/internal/provider"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

// --- hand-rolled SSE frame parsing ---

type sseFrame struct {
	event string
	data  json.RawMessage
}

// readSSEFrames parses "event: ...\ndata: ...\n\n" frames until body is
// exhausted (the server closes the connection once the handler returns).
func readSSEFrames(t *testing.T, body io.Reader) []sseFrame {
	t.Helper()
	var frames []sseFrame
	var cur sseFrame
	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		case line == "":
			if cur.event != "" {
				frames = append(frames, cur)
				cur = sseFrame{}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan SSE body: %v", err)
	}
	return frames
}

// --- a provider whose Stream yields multiple independent deltas, to prove
// several token frames reach the client in order over a real connection
// (fakeProviderFactory's scriptedProvider only yields one, via
// provider.NewResponseStream, so it can't demonstrate this on its own).

type multiDeltaStream struct {
	deltas []string
	idx    int
	resp   *provider.Response
}

func (s *multiDeltaStream) Next() bool {
	if s.idx >= len(s.deltas) {
		return false
	}
	s.idx++
	return true
}
func (s *multiDeltaStream) Delta() string                         { return s.deltas[s.idx-1] }
func (s *multiDeltaStream) Response() (*provider.Response, error) { return s.resp, nil }
func (s *multiDeltaStream) Err() error                            { return nil }
func (s *multiDeltaStream) Close() error                          { return nil }

type multiDeltaProvider struct {
	deltas []string
	resp   *provider.Response
}

func (p *multiDeltaProvider) Name() string { return "multi-delta" }
func (p *multiDeltaProvider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	return p.resp, nil
}
func (p *multiDeltaProvider) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	return &multiDeltaStream{deltas: p.deltas, resp: p.resp}, nil
}

func multiDeltaProviderFactory(deltas []string, resp *provider.Response) func(config.ModelConfig) (provider.Provider, error) {
	p := &multiDeltaProvider{deltas: deltas, resp: resp}
	return func(config.ModelConfig) (provider.Provider, error) { return p, nil }
}

// TestStreamTokensArriveInOrderOverHTTP is the phase's headline acceptance
// test for the transport: a real HTTP round trip through httptest.Server,
// asserting the SSE frames a client sees are the token deltas in order,
// followed by a done frame — not a single buffered response.
func TestStreamTokensArriveInOrderOverHTTP(t *testing.T) {
	ts := newTestServer(t, multiDeltaProviderFactory([]string{"Hel", "lo, ", "world"}, textResponse("Hello, world")))
	postAgent(t, ts, minimalYAML)

	resp, err := http.Post(ts.URL+"/v1/agents/minimal/stream", "application/json", strings.NewReader(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("POST stream: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	frames := readSSEFrames(t, resp.Body)
	if len(frames) != 4 {
		t.Fatalf("expected 3 token frames + 1 done frame, got %d: %+v", len(frames), frames)
	}
	for i, want := range []string{"Hel", "lo, ", "world"} {
		if frames[i].event != "token" {
			t.Fatalf("frame %d event = %q, want token", i, frames[i].event)
		}
		var tok tokenEvent
		if err := json.Unmarshal(frames[i].data, &tok); err != nil {
			t.Fatalf("decode token frame %d: %v", i, err)
		}
		if tok.Text != want {
			t.Fatalf("frame %d text = %q, want %q", i, tok.Text, want)
		}
	}
	if frames[3].event != "done" {
		t.Fatalf("last frame event = %q, want done", frames[3].event)
	}
	var done doneEvent
	if err := json.Unmarshal(frames[3].data, &done); err != nil {
		t.Fatalf("decode done frame: %v", err)
	}
	if done.State != "completed" {
		t.Fatalf("done.State = %q, want completed", done.State)
	}
	if done.RunID == "" {
		t.Fatal("expected a non-empty run_id on the done frame")
	}
}

// installSSESink mirrors the event-forwarding closure handleStreamAgent
// installs on a real request, so tests calling streamRun directly (below)
// see the same tool_call/tool_result frames a real client would.
func installSSESink(eng *runtime.Engine, sse *sseWriter) {
	eng.OnEvent(func(ev runtime.Event) {
		switch ev.Kind {
		case runtime.EventToken:
			sse.send("token", tokenEvent{Text: ev.Text})
		case runtime.EventToolCall:
			sse.send("tool_call", toolCallEvent{CallID: ev.CallID, Tool: ev.ToolName, Args: ev.Args})
		case runtime.EventToolResult:
			sse.send("tool_result", toolResultEvent{CallID: ev.CallID, Tool: ev.ToolName, Result: ev.Result, IsError: ev.IsError})
		}
	})
}

// TestStreamPausesAtAwaitingApprovalThenResumes covers the tool-call and
// approval acceptance criteria directly against streamRun + a
// httptest.NewRecorder, independent of npx/MCP: the stream must emit the
// tool_call as soon as the model makes it, then an awaiting_approval frame
// and stop — not hang waiting for a decision that can only come out of
// band. After approval, a fresh stream call must emit the tool_result and
// a terminal done frame.
func TestStreamPausesAtAwaitingApprovalThenResumes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := &Server{store: st, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	sp := &scriptedProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{"x":1}`),
		textResponse("done"),
	}}
	eng := runtime.NewEngine(st, sp, runtime.Config{
		AgentName: "t", Model: "m", MaxTurns: 10,
		Approvals: runtime.ApprovalPolicy{Require: []string{"danger.tool"}},
	})
	eng.RegisterTool(runtime.Tool{
		Name: "danger.tool", InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "should not run", nil
		},
	})

	runID := "run-pause"
	if err := eng.NewRun(ctx, runID, "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	rec1 := httptest.NewRecorder()
	sse1, err := newSSEWriter(rec1)
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	installSSESink(eng, sse1)
	srv.streamRun(ctx, sse1, eng, runID)

	frames := readSSEFrames(t, rec1.Body)
	if len(frames) != 2 {
		t.Fatalf("expected a tool_call frame then an awaiting_approval frame, got %d: %+v", len(frames), frames)
	}
	if frames[0].event != "tool_call" {
		t.Fatalf("frame 0 event = %q, want tool_call", frames[0].event)
	}
	var tc toolCallEvent
	if err := json.Unmarshal(frames[0].data, &tc); err != nil {
		t.Fatalf("decode tool_call frame: %v", err)
	}
	if tc.CallID != "call_1" || tc.Tool != "danger.tool" {
		t.Fatalf("tool_call frame = %+v, want call_1/danger.tool", tc)
	}
	if frames[1].event != "awaiting_approval" {
		t.Fatalf("frame 1 event = %q, want awaiting_approval — the stream must pause, not hang", frames[1].event)
	}
	var aa awaitingApprovalEvent
	if err := json.Unmarshal(frames[1].data, &aa); err != nil {
		t.Fatalf("decode awaiting_approval frame: %v", err)
	}
	if len(aa.Pending) != 1 || aa.Pending[0].CallID != "call_1" {
		t.Fatalf("expected one pending call_1, got %+v", aa.Pending)
	}

	if _, err := eng.RecordApproval(ctx, runID, "call_1", "approved", "test", ""); err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}

	rec2 := httptest.NewRecorder()
	sse2, err := newSSEWriter(rec2)
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	installSSESink(eng, sse2)
	srv.streamRun(ctx, sse2, eng, runID)

	// Resuming runs the tool (tool_result) and, since the run isn't done
	// yet, continues straight into the next model turn — which, with the
	// event sink still installed, streams as a token frame before the run
	// completes.
	frames = readSSEFrames(t, rec2.Body)
	if len(frames) != 3 {
		t.Fatalf("expected tool_result, token, done frames, got %d: %+v", len(frames), frames)
	}
	if frames[0].event != "tool_result" {
		t.Fatalf("frame 0 event = %q, want tool_result", frames[0].event)
	}
	var tr toolResultEvent
	if err := json.Unmarshal(frames[0].data, &tr); err != nil {
		t.Fatalf("decode tool_result frame: %v", err)
	}
	if tr.CallID != "call_1" || tr.IsError {
		t.Fatalf("expected a successful tool_result for call_1, got %+v", tr)
	}
	if frames[1].event != "token" {
		t.Fatalf("frame 1 event = %q, want token", frames[1].event)
	}
	if frames[2].event != "done" {
		t.Fatalf("frame 2 event = %q, want done", frames[2].event)
	}
	var done doneEvent
	if err := json.Unmarshal(frames[2].data, &done); err != nil {
		t.Fatalf("decode done frame: %v", err)
	}
	if done.State != "completed" {
		t.Fatalf("done.State = %q, want completed", done.State)
	}
}

// TestStreamFailedRunEmitsDoneNotError checks the confirmed wire-contract
// choice: a run that fails mid-stream reports through the same "done"
// event as a success, carrying state:"failed" and the error string,
// rather than a separate "error" event type.
func TestStreamFailedRunEmitsDoneNotError(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := &Server{store: st, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	sp := &scriptedProvider{responses: nil} // no scripted responses: Complete errors immediately
	eng := runtime.NewEngine(st, sp, runtime.Config{AgentName: "t", Model: "m", MaxTurns: 10})

	runID := "run-fail"
	if err := eng.NewRun(ctx, runID, "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	rec := httptest.NewRecorder()
	sse, err := newSSEWriter(rec)
	if err != nil {
		t.Fatalf("newSSEWriter: %v", err)
	}
	srv.streamRun(ctx, sse, eng, runID)

	frames := readSSEFrames(t, rec.Body)
	if len(frames) != 1 || frames[0].event != "done" {
		t.Fatalf("expected a single done frame, got %+v", frames)
	}
	var done doneEvent
	if err := json.Unmarshal(frames[0].data, &done); err != nil {
		t.Fatalf("decode done frame: %v", err)
	}
	if done.State != "failed" {
		t.Fatalf("done.State = %q, want failed", done.State)
	}
	if done.Error == nil || *done.Error == "" {
		t.Fatal("expected a non-empty error string on a failed done frame")
	}
}
