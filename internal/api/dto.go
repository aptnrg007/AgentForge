package api

import (
	"encoding/json"

	"agentforge/internal/message"
	"agentforge/internal/store"
)

type agentSummary struct {
	Name      string `json:"name"`
	YAML      string `json:"yaml"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

func toAgentSummary(a *store.Agent) agentSummary {
	return agentSummary{Name: a.Name, YAML: a.YAML, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt}
}

type toolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type runRequest struct {
	Message string `json:"message"`
}

type runResponse struct {
	RunID    string            `json:"run_id"`
	State    string            `json:"state"`
	Error    *string           `json:"error,omitempty"`
	Messages []message.Message `json:"messages,omitempty"`
}

// toolCallDTO mirrors store.ToolCall for the API surface. Args is typed as
// json.RawMessage because it's always validated JSON before it reaches the
// store; Result stays a plain string because tool output is arbitrary text
// and forcing it through RawMessage would emit invalid JSON whenever the
// text isn't itself valid JSON.
type toolCallDTO struct {
	ID         string          `json:"id"`
	ToolName   string          `json:"tool_name"`
	Args       json.RawMessage `json:"args"`
	Approval   string          `json:"approval"`
	Result     *string         `json:"result,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	CreatedAt  int64           `json:"created_at"`
	ExecutedAt *int64          `json:"executed_at,omitempty"`
}

func toToolCallDTO(tc store.ToolCall) toolCallDTO {
	return toolCallDTO{
		ID:         tc.ID,
		ToolName:   tc.ToolName,
		Args:       json.RawMessage(tc.ArgsJSON),
		Approval:   tc.Approval,
		Result:     tc.Result,
		IsError:    tc.IsError,
		CreatedAt:  tc.CreatedAt,
		ExecutedAt: tc.ExecutedAt,
	}
}

type runTrace struct {
	RunID     string            `json:"run_id"`
	AgentName string            `json:"agent_name"`
	State     string            `json:"state"`
	TurnCount int               `json:"turn_count"`
	Error     *string           `json:"error,omitempty"`
	CreatedAt int64             `json:"created_at"`
	UpdatedAt int64             `json:"updated_at"`
	Messages  []message.Message `json:"messages"`
	ToolCalls []toolCallDTO     `json:"tool_calls"`
}
