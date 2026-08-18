package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agentforge/internal/message"
)

const defaultOllamaBaseURL = "http://localhost:11434"

type Ollama struct {
	BaseURL string
	Client  *http.Client
}

func NewOllama(baseURL string) *Ollama {
	if baseURL == "" {
		baseURL = defaultOllamaBaseURL
	}
	return &Ollama{BaseURL: strings.TrimSuffix(baseURL, "/"), Client: http.DefaultClient}
}

func (o *Ollama) Name() string { return "ollama" }

// --- wire format ---

type ollamaToolCall struct {
	Function ollamaFunctionCall `json:"function"`
}

type ollamaFunctionCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaTool struct {
	Type     string             `json:"type"`
	Function ollamaToolFunction `json:"function"`
}

type ollamaToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ollamaChatRequest struct {
	Model    string           `json:"model"`
	Messages []ollamaMessage  `json:"messages"`
	Tools    []ollamaTool     `json:"tools,omitempty"`
	Stream   bool             `json:"stream"`
	Options  ollamaOptions    `json:"options,omitempty"`
}

type ollamaOptions struct {
	Temperature float64 `json:"temperature,omitempty"`
	NumPredict  int     `json:"num_predict,omitempty"`
}

type ollamaChatResponse struct {
	Message    ollamaMessage `json:"message"`
	Done       bool          `json:"done"`
	DoneReason string        `json:"done_reason"`
}

// --- translation ---

func toOllamaMessages(system string, msgs []message.Message) []ollamaMessage {
	var out []ollamaMessage
	if system != "" {
		out = append(out, ollamaMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case message.RoleTool:
			// Ollama expects one message per tool result.
			for _, b := range m.Content {
				if b.Type == message.BlockToolResult {
					content := b.Content
					if b.IsError {
						content = "ERROR: " + content
					}
					out = append(out, ollamaMessage{Role: "tool", Content: content})
				}
			}
		default:
			var text strings.Builder
			var calls []ollamaToolCall
			for _, b := range m.Content {
				switch b.Type {
				case message.BlockText:
					text.WriteString(b.Text)
				case message.BlockToolUse:
					var args map[string]any
					_ = json.Unmarshal(b.Input, &args)
					calls = append(calls, ollamaToolCall{Function: ollamaFunctionCall{Name: b.Name, Arguments: args}})
				}
			}
			out = append(out, ollamaMessage{Role: string(m.Role), Content: text.String(), ToolCalls: calls})
		}
	}
	return out
}

func toOllamaTools(tools []ToolDef) []ollamaTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ollamaTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, ollamaTool{
			Type: "function",
			Function: ollamaToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return out
}

func fromOllamaMessage(m ollamaMessage) []message.ContentBlock {
	var blocks []message.ContentBlock
	if m.Content != "" {
		blocks = append(blocks, message.ContentBlock{Type: message.BlockText, Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		input, err := json.Marshal(tc.Function.Arguments)
		if err != nil {
			input = json.RawMessage("{}")
		}
		blocks = append(blocks, message.ContentBlock{
			Type:  message.BlockToolUse,
			ID:    newToolCallID(),
			Name:  tc.Function.Name,
			Input: input,
		})
	}
	return blocks
}

func newToolCallID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("toolu_%x", b)
}

func (o *Ollama) Complete(ctx context.Context, r Request) (*Response, error) {
	body := ollamaChatRequest{
		Model:    r.Model,
		Messages: toOllamaMessages(r.System, r.Messages),
		Tools:    toOllamaTools(r.Tools),
		Stream:   false,
		Options: ollamaOptions{
			Temperature: r.Temperature,
			NumPredict:  r.MaxTokens,
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("ollama: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("ollama: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("ollama: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama: status %d: %s", resp.StatusCode, string(respBody))
	}

	var chatResp ollamaChatResponse
	if err := json.Unmarshal(respBody, &chatResp); err != nil {
		return nil, fmt.Errorf("ollama: decode response: %w", err)
	}

	blocks := fromOllamaMessage(chatResp.Message)
	stopReason := "end_turn"
	for _, b := range blocks {
		if b.Type == message.BlockToolUse {
			stopReason = "tool_use"
			break
		}
	}

	return &Response{Content: blocks, StopReason: stopReason}, nil
}
