package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"agentforge/internal/agent"
	"agentforge/internal/config"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

const maxBodyBytes = 1 << 20 // 1MiB

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleCreateAgent creates or updates an agent from a raw YAML body. The
// agent's name comes from the YAML's own `name:` field, not the URL, per
// docs/DESIGN.md section 9. The YAML is fully parsed and validated before
// it's persisted, so a bad config never reaches the store.
func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	cfg, err := config.Parse(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	if err := s.store.UpsertAgent(r.Context(), cfg.Name, string(body)); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	ag, err := s.store.GetAgent(r.Context(), cfg.Name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, toAgentSummary(ag))
}

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.store.ListAgents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]agentSummary, len(agents))
	for i := range agents {
		resp[i] = toAgentSummary(&agents[i])
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	ag, err := s.store.GetAgent(r.Context(), r.PathValue("name"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAgentSummary(ag))
}

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteAgent(r.Context(), r.PathValue("name")); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentTools returns the agent's resolved, filtered, namespaced tool
// list: it connects to every configured MCP server, namespaces each tool
// "<server>.<tool>", and applies the tools: glob filter — the same
// resolution a run would use, without actually running anything.
func (s *Server) handleAgentTools(w http.ResponseWriter, r *http.Request) {
	ag, err := s.store.GetAgent(r.Context(), r.PathValue("name"))
	if err != nil {
		writeStoreError(w, err)
		return
	}

	cfg, err := config.Parse([]byte(ag.YAML))
	if err != nil {
		// The stored YAML was validated at creation time, so this would
		// only happen if it were hand-edited in the DB out of band.
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	tools, err := agent.ResolveTools(r.Context(), s.registry, cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("resolve tools: %w", err))
		return
	}

	out := make([]toolInfo, len(tools))
	for i, t := range tools {
		out[i] = toolInfo{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRunAgent runs the agent synchronously, stepping the engine until it
// hits a terminal state or awaiting_approval (docs/DESIGN.md section 9).
func (s *Server) handleRunAgent(w http.ResponseWriter, r *http.Request) {
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

	state, err := driveToStopPoint(ctx, eng, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	resp, status, err := s.buildRunResponse(ctx, runID, state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, status, resp)
}

// handleApprove records a decision on one pending tool call. It does not by
// itself continue the run — call /resume for that — so a caller can approve
// several pending calls before driving the engine forward again.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := r.PathValue("id")

	var req approveRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if req.CallID == "" || (req.Decision != "approved" && req.Decision != "denied") {
		writeError(w, http.StatusBadRequest, fmt.Errorf(`call_id is required and decision must be "approved" or "denied"`))
		return
	}

	eng, run, err := s.buildEngineForRun(ctx, runID)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	// No auth/identity in v0.1 (docs/DESIGN.md section 9), so there's no
	// real "who" to record beyond "came in over the API".
	state, err := eng.RecordApproval(ctx, run.ID, req.CallID, req.Decision, "api", req.Reason)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"run_id": run.ID, "state": string(state)})
}

// handleResume continues a run — most commonly one sitting in
// awaiting_approval whose pending calls have all been decided — stepping
// the engine until it hits a terminal state or awaiting_approval again.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := r.PathValue("id")

	eng, run, err := s.buildEngineForRun(ctx, runID)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	state, err := driveToStopPoint(ctx, eng, run.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	resp, status, err := s.buildRunResponse(ctx, run.ID, state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, status, resp)
}

// handleCancel stops a non-terminal run immediately. Deliberately doesn't
// go through buildEngineForRun the way approve/resume do: Engine.Cancel
// only touches persisted run state (store.GetRun/UpdateRun), never the
// provider or tools, so it doesn't need a real one — and building one
// anyway would mean a run stuck behind a since-broken agent config (a
// revoked API key, a YAML edit that no longer parses) couldn't be
// cancelled either, which defeats the point of an escape hatch.
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := r.PathValue("id")

	eng := runtime.NewEngine(s.store, nil, runtime.Config{})
	state, err := eng.Cancel(ctx, runID)
	if err != nil {
		if errors.Is(err, runtime.ErrAlreadyTerminal) {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"run_id": runID, "state": string(state)})
}

// buildEngineForRun reconstructs the engine that owns runID from its
// agent's persisted config — consistent with the state machine's design
// (docs/DESIGN.md ground rule 1): nothing about a run lives only in memory,
// so any handler can pick it back up from the store alone.
func (s *Server) buildEngineForRun(ctx context.Context, runID string) (*runtime.Engine, *store.Run, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return nil, nil, err
	}
	ag, err := s.store.GetAgent(ctx, run.AgentName)
	if err != nil {
		return nil, nil, err
	}
	if ag.YAML == "" {
		// Engine.NewRun's EnsureAgentExists inserts a placeholder row (empty
		// YAML) to satisfy runs.agent_name's foreign key when a run starts
		// for an agent that was never registered via UpsertAgent — every
		// current caller (cli run/chat, this API's POST /v1/agents/run
		// flow) upserts real config first, so this shouldn't happen in
		// practice, but a config.Parse("") failure ("name is required")
		// would otherwise look like a corrupted YAML file rather than what
		// it actually is: this agent has no stored config to resume from.
		return nil, nil, fmt.Errorf("agent %q has no stored config (run %s cannot be resumed)", run.AgentName, run.ID)
	}
	cfg, err := config.Parse([]byte(ag.YAML))
	if err != nil {
		return nil, nil, err
	}
	eng, err := agent.Build(ctx, s.store, s.registry, cfg, s.providerFactory)
	if err != nil {
		return nil, nil, err
	}
	return eng, run, nil
}

// buildRunResponse assembles the response body for a run at its current
// stop point, including the pending-approval list when applicable.
func (s *Server) buildRunResponse(ctx context.Context, runID string, state runtime.State) (runResponse, int, error) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return runResponse{}, 0, err
	}
	msgs, err := s.store.ListMessages(ctx, runID)
	if err != nil {
		return runResponse{}, 0, err
	}

	resp := runResponse{RunID: runID, State: string(state), Error: run.Error, Messages: msgs}
	status := http.StatusOK

	if state == runtime.StateAwaitingApproval {
		status = http.StatusAccepted
		pending, err := s.store.ListPendingApprovals(ctx, runID)
		if err != nil {
			return runResponse{}, 0, err
		}
		resp.Pending = make([]pendingCall, len(pending))
		for i, tc := range pending {
			resp.Pending[i] = pendingCall{CallID: tc.ID, Tool: tc.ToolName, Args: json.RawMessage(tc.ArgsJSON)}
		}
	}

	return resp, status, nil
}

// driveToStopPoint steps the engine until it reaches a terminal state or
// needs a human decision.
func driveToStopPoint(ctx context.Context, eng *runtime.Engine, runID string) (runtime.State, error) {
	for {
		// A client disconnect cancels r.Context() mid-run — end the run
		// rather than calling Step with a ctx already known to be dead
		// (which would just fail at its own GetRun for the same reason)
		// and leaving it stuck non-terminal with no one left to read the
		// response anyway. See runtime.Engine.EndIfContextDone and
		// internal/cli/localrun.go's matching check.
		if ctx.Err() != nil {
			if state := eng.EndIfContextDone(ctx, runID); state != "" {
				return state, ctx.Err()
			}
			return "", ctx.Err()
		}

		// Step self-heals a ctx that dies *during* this call (see its own
		// dead-ctx recovery), so a non-nil error here is a real failure,
		// not a run left stranded non-terminal.
		state, err := eng.Step(ctx, runID)
		if err != nil {
			return "", err
		}
		switch state {
		case runtime.StateCompleted, runtime.StateFailed, runtime.StateCancelled, runtime.StateAwaitingApproval:
			return state, nil
		}
	}
}

// handleListRuns returns runs most-recent-first, optionally filtered to
// one agent via ?agent= and capped via ?limit= (default 20).
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid limit %q", v))
			return
		}
		limit = n
	}

	runs, err := s.store.ListRuns(r.Context(), r.URL.Query().Get("agent"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]runSummary, len(runs))
	for i, run := range runs {
		out[i] = toRunSummary(run)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	run, err := s.store.GetRun(ctx, id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	msgs, err := s.store.ListMessagesDetailed(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	calls, err := s.store.ListToolCalls(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	messages := make([]messageDTO, len(msgs))
	for i, m := range msgs {
		messages[i] = toMessageDTO(m)
	}
	toolCalls := make([]toolCallDTO, len(calls))
	for i, c := range calls {
		toolCalls[i] = toToolCallDTO(c)
	}

	writeJSON(w, http.StatusOK, runTrace{
		RunID:     run.ID,
		AgentName: run.AgentName,
		State:     run.State,
		TurnCount: run.TurnCount,
		Error:     run.Error,
		CreatedAt: run.CreatedAt,
		UpdatedAt: run.UpdatedAt,
		Messages:  messages,
		ToolCalls: toolCalls,
	})
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrNotPending):
		writeError(w, http.StatusConflict, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}
