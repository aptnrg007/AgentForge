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
	return &Ollama{BaseURL: strings.TrimSuffix(baseURL, "/"), Client: newHTTPClient()}
}

func (o *Ollama) Name() string { return "ollama" }

func (o *Ollama) Capabilities() Capabilities {
	return Capabilities{ToolUse: true, StructuredOutput: true}
}

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
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []ollamaTool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Options  ollamaOptions   `json:"options,omitempty"`
	// Format requests constrained decoding: either the literal string
	// "json" or (what we send) a full JSON Schema object. Left unset
	// (omitempty) whenever the engine isn't asking for native structured
	// output, e.g. when the agent has tools registered — constrained
	// decoding to a schema makes tool calls impossible on that turn.
	Format json.RawMessage `json:"format,omitempty"`
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

// ollamaFormat returns the schema to send as the request's native-format
// constraint, or nil. Ollama's constrained decoding makes tool calls
// impossible on the turn it's used, so it's only sent when the agent has
// no tools registered on this request — a tool-using agent always takes
// the validate-and-retry fallback instead, the same outcome a provider
// with no native support at all produces. (This is Ollama's own
// limitation, not a rule every provider shares — see openai.go, which can
// combine the two.)
func ollamaFormat(r Request) json.RawMessage {
	if len(r.Tools) != 0 {
		return nil
	}
	return r.ResponseSchema
}

// doRequest marshals body and POSTs it to /api/chat, shared by Complete
// (Stream: false) and Stream (Stream: true) below — everything about the
// request is identical except that one field and what each does with the
// response afterward.
func (o *Ollama) doRequest(ctx context.Context, body ollamaChatRequest) (*http.Response, error) {
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
		return nil, newTransportError("ollama", err)
	}
	return resp, nil
}

// ollamaErrorMessage is Ollama's error body as-is: unlike the other three
// providers, it isn't a structured {"error": "..."} JSON envelope worth
// parsing out, just plain text.
func ollamaErrorMessage(body []byte) string { return string(body) }

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
		Format: ollamaFormat(r),
	}

	resp, err := o.doRequest(ctx, body)
	if err != nil {
		return nil, err
	}
	var chatResp ollamaChatResponse
	if err := decodeResponse("ollama", resp, ollamaErrorMessage, nil, &chatResp); err != nil {
		return nil, err
	}

	blocks := fromOllamaMessage(chatResp.Message)
	stopReason := responseStopReason(blocks)

	return &Response{Content: blocks, StopReason: stopReason}, nil
}

func responseStopReason(blocks []message.ContentBlock) string {
	for _, b := range blocks {
		if b.Type == message.BlockToolUse {
			return "tool_use"
		}
	}
	return "end_turn"
}

// Stream is a second, independent implementation of the chat request —
// deliberately not sharing code with Complete's response handling — that
// sets stream:true and decodes the NDJSON body one chunk at a time.
func (o *Ollama) Stream(ctx context.Context, r Request) (Stream, error) {
	body := ollamaChatRequest{
		Model:    r.Model,
		Messages: toOllamaMessages(r.System, r.Messages),
		Tools:    toOllamaTools(r.Tools),
		Stream:   true,
		Options: ollamaOptions{
			Temperature: r.Temperature,
			NumPredict:  r.MaxTokens,
		},
		Format: ollamaFormat(r),
	}

	resp, err := o.doRequest(ctx, body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, newStatusError("ollama", resp, respBody, ollamaErrorMessage(respBody), nil)
	}

	// json.Decoder, not bufio.Scanner: Scanner's default line-length cap
	// (64KB) can be exceeded by a single NDJSON chunk on a long response.
	return &ollamaStream{dec: json.NewDecoder(resp.Body), body: resp.Body}, nil
}

// ollamaStream accumulates NDJSON chunks from a streaming /api/chat
// response. Ollama sends text as successive deltas in chunk.Message.Content
// and (in practice) tool calls as a single populated ToolCalls slice in one
// of the chunks, so both are accumulated across the whole stream and only
// assembled into a final Response once the stream ends.
type ollamaStream struct {
	dec  *json.Decoder
	body io.ReadCloser

	curDelta string
	text     strings.Builder
	calls    []ollamaToolCall

	done bool
	err  error
}

func (s *ollamaStream) Next() bool {
	if s.err != nil || s.done {
		return false
	}
	var chunk ollamaChatResponse
	if err := s.dec.Decode(&chunk); err != nil {
		if err == io.EOF {
			s.done = true
			return false
		}
		s.err = fmt.Errorf("ollama: decode stream chunk: %w", err)
		return false
	}
	s.curDelta = chunk.Message.Content
	s.text.WriteString(chunk.Message.Content)
	if len(chunk.Message.ToolCalls) > 0 {
		s.calls = append(s.calls, chunk.Message.ToolCalls...)
	}
	if chunk.Done {
		s.done = true
	}
	return true
}

func (s *ollamaStream) Delta() string { return s.curDelta }
func (s *ollamaStream) Err() error    { return s.err }

func (s *ollamaStream) Response() (*Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	blocks := fromOllamaMessage(ollamaMessage{Content: s.text.String(), ToolCalls: s.calls})
	return &Response{Content: blocks, StopReason: responseStopReason(blocks)}, nil
}

func (s *ollamaStream) Close() error { return s.body.Close() }
