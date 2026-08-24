package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/runtime"
)

// handleStreamAgent behaves like handleRunAgent but reports progress over
// Server-Sent Events instead of returning a single JSON body once the run
// stops: token deltas as the model generates them, tool_call/tool_result
// as they happen, and a terminal awaiting_approval or done frame when the
// run reaches a stop point.
func (s *Server) handleStreamAgent(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := r.PathValue("name")

	ag, err := s.store.GetAgent(ctx, name)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	var req runRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("message is required"))
		return
	}

	cfg, err := config.Parse([]byte(ag.YAML))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	eng, err := agent.Build(ctx, s.store, s.registry, cfg, s.providerFactory)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}

	runID := newRunID()
	if err := eng.NewRun(ctx, runID, req.Message); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	sse, err := newSSEWriter(w)
	if err != nil {
		// Headers not yet committed at this point, so a normal JSON error
		// is still possible.
		writeError(w, http.StatusInternalServerError, err)
		return
	}

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

	s.streamRun(ctx, sse, eng, runID)
}

// streamRun steps eng until it reaches a stop point and sends the
// corresponding terminal SSE frame there. It deliberately does not share
// code with driveToStopPoint: that function (and handleRunAgent /
// handleResume, which depend on it) are left exactly as they were before
// streaming existed.
func (s *Server) streamRun(ctx context.Context, sse *sseWriter, eng *runtime.Engine, runID string) {
	for {
		// A client disconnect cancels ctx mid-run, the same way it would
		// for the non-streaming handlers — see driveToStopPoint. Checked
		// before Step, not just via sse.Err() below, since ctx can die
		// during a store call that has no SSE write anywhere near it.
		// Nothing left to report to either way — the client is gone — so
		// the resulting state is discarded; this just makes sure the run
		// still ends up terminal instead of stranded.
		if ctx.Err() != nil {
			eng.EndIfContextDone(ctx, runID)
			return
		}

		// Step self-heals a ctx that dies *during* this call (see its own
		// dead-ctx recovery), so a non-nil error here is a real failure,
		// not a run left stranded non-terminal.
		state, err := eng.Step(ctx, runID)
		if err != nil {
			errStr := err.Error()
			sse.send("done", doneEvent{RunID: runID, State: string(runtime.StateFailed), Error: &errStr})
			return
		}
		if sse.Err() != nil {
			return // client disconnected; nothing left to report to
		}

		switch state {
		case runtime.StateAwaitingApproval:
			pending, err := s.store.ListPendingApprovals(ctx, runID)
			if err != nil {
				errStr := err.Error()
				sse.send("done", doneEvent{RunID: runID, State: string(runtime.StateFailed), Error: &errStr})
				return
			}
			dtoPending := make([]pendingCall, len(pending))
			for i, tc := range pending {
				dtoPending[i] = pendingCall{CallID: tc.ID, Tool: tc.ToolName, Args: json.RawMessage(tc.ArgsJSON)}
			}
			sse.send("awaiting_approval", awaitingApprovalEvent{RunID: runID, Pending: dtoPending})
			return
		case runtime.StateCompleted, runtime.StateFailed, runtime.StateCancelled:
			run, err := s.store.GetRun(ctx, runID)
			if err != nil {
				sse.send("done", doneEvent{RunID: runID, State: string(state)})
				return
			}
			sse.send("done", doneEvent{RunID: runID, State: string(state), Error: run.Error})
			return
		}
	}
}
