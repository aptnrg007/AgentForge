package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

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
// PLAN.md section 9. The YAML is fully parsed and validated before it's
// persisted, so a bad config never reaches the store.
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
// hits a terminal state or awaiting_approval (PLAN.md section 9).
func (s *Server) handleRunAgent(w http.ResponseWriter, r *http.Request) {
	ag, err := s.store.GetAgent(r.Context(), r.PathValue("name"))
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

	ctx := r.Context()
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

	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	msgs, err := s.store.ListMessages(ctx, runID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	status := http.StatusOK
	if state == runtime.StateAwaitingApproval {
		status = http.StatusAccepted
	}
	writeJSON(w, status, runResponse{RunID: runID, State: string(state), Error: run.Error, Messages: msgs})
}

// driveToStopPoint steps the engine until it reaches a terminal state or
// needs a human decision.
func driveToStopPoint(ctx context.Context, eng *runtime.Engine, runID string) (runtime.State, error) {
	for {
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

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")

	run, err := s.store.GetRun(ctx, id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	msgs, err := s.store.ListMessages(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	calls, err := s.store.ListToolCalls(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
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
		Messages:  msgs,
		ToolCalls: toolCalls,
	})
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}
