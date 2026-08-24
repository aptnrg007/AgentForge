package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agentforge/internal/message"
)

// defaultOpenAIBaseURL includes the version segment ("/v1"), unlike
// anthropic.go's bare host — every OpenAI-compatible vendor (Groq,
// Together, Fireworks, OpenRouter, DeepSeek, xAI, Mistral, vLLM,
// llama.cpp, LM Studio) documents its base_url with the version already
// on it, so matching that convention is what makes "point base_url at a
// different host" cover the whole ecosystem with zero new code.
const defaultOpenAIBaseURL = "https://api.openai.com/v1"

type OpenAI struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

func NewOpenAI(apiKey, baseURL string) *OpenAI {
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	return &OpenAI{APIKey: apiKey, BaseURL: strings.TrimSuffix(baseURL, "/"), Client: newHTTPClient()}
}

func (o *OpenAI) Name() string { return "openai" }

func (o *OpenAI) Capabilities() Capabilities {
	return Capabilities{Vision: true, ToolUse: true, StructuredOutput: true}
}

// --- wire format (Chat Completions API) ---

type openAIFunctionCall struct {
	Name string `json:"name"`
	// Arguments is a JSON-encoded string, not an object — the one detail
	// that most differs from Ollama (map[string]any) and Anthropic (raw
	// object) and is easiest to port wrong in both directions.
	Arguments string `json:"arguments"`
}

type openAIToolCall struct {
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"` // "function"
	Function openAIFunctionCall `json:"function"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`

	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"` // assistant only

	// ToolCallID matches a result back to the call that produced it
	// (role: "tool" only) — unlike Ollama, which throws real IDs away
	// and matches purely by message order, so this must carry the
	// original tool_use block's ID verbatim.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type openAIFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openAITool struct {
	Type     string            `json:"type"` // "function"
	Function openAIFunctionDef `json:"function"`
}

// openAIJSONSchema requests native structured output. Strict is
// deliberately left false: strict:true requires every object in the
// schema to set additionalProperties:false and list every property as
// required, a subset a user-authored output.schema won't generally
// satisfy — and a violation there is a hard 400, not a soft warning.
// With strict:false the schema still steers generation, and item 2's
// validate-and-retry loop (which runs regardless of Native) remains the
// actual correctness guarantee.
type openAIJSONSchema struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict"`
}

type openAIResponseFormat struct {
	Type       string           `json:"type"` // "json_schema"
	JSONSchema openAIJSONSchema `json:"json_schema"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIChatRequest struct {
	Model          string                `json:"model"`
	Messages       []openAIMessage       `json:"messages"`
	Tools          []openAITool          `json:"tools,omitempty"`
	Temperature    float64               `json:"temperature,omitempty"`
	MaxTokens      int                   `json:"max_tokens,omitempty"`
	Stream         bool                  `json:"stream,omitempty"`
	StreamOptions  *openAIStreamOptions  `json:"stream_options,omitempty"`
	ResponseFormat *openAIResponseFormat `json:"response_format,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type openAIChoice struct {
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type openAIChatResponse struct {
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

type openAIErrorBody struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// --- translation ---

func toOpenAIMessages(system string, msgs []message.Message) []openAIMessage {
	var out []openAIMessage
	if system != "" {
		out = append(out, openAIMessage{Role: "system", Content: system})
	}
	for _, m := range msgs {
		switch m.Role {
		case message.RoleTool:
			// One wire message per tool result, matched back to its call
			// by the real tool_call_id (see openAIMessage.ToolCallID).
			for _, b := range m.Content {
				if b.Type != message.BlockToolResult {
					continue
				}
				content := b.Content
				if b.IsError {
					content = "ERROR: " + content
				}
				out = append(out, openAIMessage{Role: "tool", Content: content, ToolCallID: b.ToolUseID})
			}
		default:
			var text strings.Builder
			var calls []openAIToolCall
			for _, b := range m.Content {
				switch b.Type {
				case message.BlockText:
					text.WriteString(b.Text)
				case message.BlockToolUse:
					input := b.Input
					if len(input) == 0 {
						input = json.RawMessage("{}")
					}
					calls = append(calls, openAIToolCall{
						ID:       b.ID,
						Type:     "function",
						Function: openAIFunctionCall{Name: toWireToolName(b.Name), Arguments: string(input)},
					})
				}
			}
			out = append(out, openAIMessage{Role: string(m.Role), Content: text.String(), ToolCalls: calls})
		}
	}
	return out
}

func toOpenAITools(tools []ToolDef) []openAITool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]openAITool, 0, len(tools))
	for _, t := range tools {
		out = append(out, openAITool{
			Type: "function",
			Function: openAIFunctionDef{
				Name:        toWireToolName(t.Name),
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return out
}

func openAIResponseFormatFor(schema json.RawMessage) *openAIResponseFormat {
	if len(schema) == 0 {
		return nil
	}
	return &openAIResponseFormat{
		Type:       "json_schema",
		JSONSchema: openAIJSONSchema{Name: "output", Schema: schema, Strict: false},
	}
}

func fromOpenAIMessage(m openAIMessage) []message.ContentBlock {
	var blocks []message.ContentBlock
	if m.Content != "" {
		blocks = append(blocks, message.ContentBlock{Type: message.BlockText, Text: m.Content})
	}
	for _, tc := range m.ToolCalls {
		input := json.RawMessage(tc.Function.Arguments)
		if len(input) == 0 || !json.Valid(input) {
			input = json.RawMessage("{}")
		}
		id := tc.ID
		if id == "" {
			// Defensive: a spec-conformant server always sends one, but
			// an OpenAI-compatible endpoint that doesn't shouldn't crash
			// the run — same fallback Ollama always takes.
			id = newToolCallID()
		}
		blocks = append(blocks, message.ContentBlock{
			Type: message.BlockToolUse, ID: id, Name: fromWireToolName(tc.Function.Name), Input: input,
		})
	}
	return blocks
}

// normalizeOpenAIStopReason maps finish_reason onto our
// end_turn/tool_use/max_tokens vocabulary (see
// normalizeAnthropicStopReason for the Anthropic equivalent). Anything
// else passes through unchanged rather than being silently lost.
func normalizeOpenAIStopReason(fr string) string {
	switch fr {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return fr
	}
}

func openAIErrorMessage(body []byte) string {
	var eb openAIErrorBody
	if err := json.Unmarshal(body, &eb); err == nil && eb.Error.Message != "" {
		return eb.Error.Message
	}
	return string(body)
}

func (o *OpenAI) buildRequest(r Request, stream bool) openAIChatRequest {
	req := openAIChatRequest{
		Model:          r.Model,
		Messages:       toOpenAIMessages(r.System, r.Messages),
		Tools:          toOpenAITools(r.Tools),
		Temperature:    r.Temperature,
		MaxTokens:      r.MaxTokens,
		Stream:         stream,
		ResponseFormat: openAIResponseFormatFor(r.ResponseSchema),
	}
	if stream {
		// Without stream_options, usage never arrives in a streamed
		// response at all — the final chunk just omits it silently.
		req.StreamOptions = &openAIStreamOptions{IncludeUsage: true}
	}
	return req
}

func (o *OpenAI) doRequest(ctx context.Context, body openAIChatRequest) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openai: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)

	resp, err := o.Client.Do(req)
	if err != nil {
		return nil, newTransportError("openai", err)
	}
	return resp, nil
}

func (o *OpenAI) Complete(ctx context.Context, r Request) (*Response, error) {
	resp, err := o.doRequest(ctx, o.buildRequest(r, false))
	if err != nil {
		return nil, err
	}
	var cr openAIChatResponse
	if err := decodeResponse("openai", resp, openAIErrorMessage, nil, &cr); err != nil {
		return nil, err
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("openai: response has no choices")
	}
	choice := cr.Choices[0]

	return &Response{
		Content:    fromOpenAIMessage(choice.Message),
		StopReason: normalizeOpenAIStopReason(choice.FinishReason),
		Usage:      Usage{InputTokens: cr.Usage.PromptTokens, OutputTokens: cr.Usage.CompletionTokens},
	}, nil
}

// Stream sends the same request with stream:true and adapts the Chat
// Completions SSE format — bare "data: {...}" lines (no event: field),
// terminated by the literal "data: [DONE]" — onto the pull-based Stream
// interface.
func (o *OpenAI) Stream(ctx context.Context, r Request) (Stream, error) {
	resp, err := o.doRequest(ctx, o.buildRequest(r, true))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, newStatusError("openai", resp, respBody, openAIErrorMessage(respBody), nil)
	}
	return &openAIStream{
		body:      resp.Body,
		reader:    newSSEDataReader(resp.Body),
		toolCalls: map[int]*openAIToolCallAccum{},
	}, nil
}

// --- stream assembly ---

// openAIStreamChunk covers the fields used from a Chat Completions
// streaming chunk. Tool-call argument fragments arrive keyed by Index,
// not accumulated server-side, hence the per-index accumulator below —
// the same shape anthropicContentAccum uses for input_json_delta.
type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int                `json:"index"`
				ID       string             `json:"id"`
				Function openAIFunctionCall `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
}

type openAIToolCallAccum struct {
	id      string
	name    string
	argsBuf strings.Builder
}

type openAIStream struct {
	body   io.ReadCloser
	reader *sseDataReader

	text      strings.Builder
	toolCalls map[int]*openAIToolCallAccum
	toolOrder []int

	stopReason string
	usage      Usage

	curDelta string
	done     bool
	err      error
}

// Next reads exactly one SSE frame per call — every path below returns, so
// this was previously wrapped in a `for {}` that could never actually
// iterate a second time; removed rather than left as dead loop syntax.
func (s *openAIStream) Next() bool {
	if s.err != nil || s.done {
		return false
	}
	data, ok := s.reader.next()
	if !ok {
		if err := s.reader.err(); err != nil {
			s.err = fmt.Errorf("openai: read stream: %w", err)
		} else {
			s.err = fmt.Errorf("openai: stream ended without [DONE]")
		}
		return false
	}

	s.curDelta = ""
	if string(data) == "[DONE]" {
		s.done = true
		return true
	}

	var chunk openAIStreamChunk
	if err := json.Unmarshal(data, &chunk); err != nil {
		s.err = fmt.Errorf("openai: decode stream chunk: %w", err)
		return false
	}
	if chunk.Usage != nil {
		s.usage = Usage{InputTokens: chunk.Usage.PromptTokens, OutputTokens: chunk.Usage.CompletionTokens}
	}
	if len(chunk.Choices) == 0 {
		// The final usage-bearing chunk (stream_options.include_usage)
		// carries an empty choices array — not an error, just nothing
		// else to do with this frame.
		return true
	}

	choice := chunk.Choices[0]
	if choice.FinishReason != "" {
		s.stopReason = choice.FinishReason
	}
	if choice.Delta.Content != "" {
		s.text.WriteString(choice.Delta.Content)
		s.curDelta = choice.Delta.Content
	}
	for _, tc := range choice.Delta.ToolCalls {
		acc, ok := s.toolCalls[tc.Index]
		if !ok {
			acc = &openAIToolCallAccum{}
			s.toolCalls[tc.Index] = acc
			s.toolOrder = append(s.toolOrder, tc.Index)
		}
		if tc.ID != "" {
			acc.id = tc.ID
		}
		if tc.Function.Name != "" {
			acc.name = tc.Function.Name
		}
		acc.argsBuf.WriteString(tc.Function.Arguments)
	}
	return true
}

func (s *openAIStream) Delta() string { return s.curDelta }
func (s *openAIStream) Err() error    { return s.err }

func (s *openAIStream) Response() (*Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	var blocks []message.ContentBlock
	if t := s.text.String(); t != "" {
		blocks = append(blocks, message.ContentBlock{Type: message.BlockText, Text: t})
	}
	for _, idx := range s.toolOrder {
		acc := s.toolCalls[idx]
		input := json.RawMessage(acc.argsBuf.String())
		if len(input) == 0 || !json.Valid(input) {
			input = json.RawMessage("{}")
		}
		id := acc.id
		if id == "" {
			id = newToolCallID()
		}
		blocks = append(blocks, message.ContentBlock{
			Type: message.BlockToolUse, ID: id, Name: fromWireToolName(acc.name), Input: input,
		})
	}
	return &Response{Content: blocks, StopReason: normalizeOpenAIStopReason(s.stopReason), Usage: s.usage}, nil
}

func (s *openAIStream) Close() error { return s.body.Close() }
