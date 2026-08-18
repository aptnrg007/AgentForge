package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"agentforge/internal/message"
)

// These mirror the JSON shapes internal/api's handlers produce. They're
// duplicated rather than imported because api's DTOs are unexported (the
// API's wire format is the contract, not the Go types behind it), and a
// CLI HTTP client decoding response JSON is normal client code, not a
// layering shortcut.

type remoteAgent struct {
	Name      string `json:"name"`
	YAML      string `json:"yaml"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type remoteRun struct {
	RunID    string            `json:"run_id"`
	State    string            `json:"state"`
	Error    *string           `json:"error,omitempty"`
	Messages []message.Message `json:"messages,omitempty"`
}

type remoteToolCall struct {
	ID       string          `json:"id"`
	ToolName string          `json:"tool_name"`
	Args     json.RawMessage `json:"args"`
	Approval string          `json:"approval"`
	Result   *string         `json:"result,omitempty"`
	IsError  bool            `json:"is_error,omitempty"`
}

type remoteRunTrace struct {
	RunID     string            `json:"run_id"`
	State     string            `json:"state"`
	TurnCount int               `json:"turn_count"`
	Error     *string           `json:"error,omitempty"`
	Messages  []message.Message `json:"messages"`
	ToolCalls []remoteToolCall  `json:"tool_calls"`
}

type apiErrorBody struct {
	Error string `json:"error"`
}

func apiGet(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	return doAPIRequest(req, out)
}

func apiPost(ctx context.Context, url, contentType string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	return doAPIRequest(req, out)
}

func apiDelete(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	return doAPIRequest(req, nil)
}

func doAPIRequest(req *http.Request, out any) error {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 300 {
		var eb apiErrorBody
		if json.Unmarshal(body, &eb) == nil && eb.Error != "" {
			return fmt.Errorf("server returned %d: %s", resp.StatusCode, eb.Error)
		}
		return fmt.Errorf("server returned %d: %s", resp.StatusCode, string(body))
	}

	if out != nil && len(body) > 0 {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
