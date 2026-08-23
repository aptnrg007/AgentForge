package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agentforge/internal/message"
)

const (
	defaultAnthropicBaseURL   = "https://api.anthropic.com"
	anthropicAPIVersion       = "2023-06-01"
	defaultAnthropicMaxTokens = 4096 // Anthropic requires max_tokens on every request; ours defaults to 0 (unset)
)

type Anthropic struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

func NewAnthropic(apiKey, baseURL string) *Anthropic {
	if baseURL == "" {
		baseURL = defaultAnthropicBaseURL
	}
	return &Anthropic{APIKey: apiKey, BaseURL: strings.TrimSuffix(baseURL, "/"), Client: http.DefaultClient}
}

func (a *Anthropic) Name() string { return "anthropic" }

func (a *Anthropic) Capabilities() Capabilities {
	return Capabilities{Vision: true, ToolUse: true}
}

// --- wire format (Anthropic Messages API) ---

// anthropicContent covers every content-block shape the Messages API
// uses across both requests and responses (text, tool_use, tool_result);
// omitempty keeps each wire object down to only the fields its Type uses.
type anthropicContent struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result — travels in a "user"-role message immediately after
	// the assistant turn that produced the tool_use it references.
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

type anthropicMessage struct {
	Role    string             `json:"role"`
	Content []anthropicContent `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Temperature float64            `json:"temperature,omitempty"`
	Stream      bool               `json:"stream,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type anthropicResponse struct {
	Content    []anthropicContent `json:"content"`
	StopReason string             `json:"stop_reason"`
	Usage      anthropicUsage     `json:"usage"`
}

type anthropicErrorBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// --- translation ---

// toAnthropicMessages maps our internal messages onto Anthropic's shape.
// The runtime only ever appends messages in the order
// user, assistant, [tool(=user), assistant]* (Engine.stepModel/stepTools),
// so role alternation — which Anthropic requires — holds by construction;
// this only has to translate roles and content blocks, not reorder them.
func toAnthropicMessages(msgs []message.Message) []anthropicMessage {
	var out []anthropicMessage
	for _, m := range msgs {
		role := "user"
		if m.Role == message.RoleAssistant {
			role = "assistant"
		}
		// message.RoleTool travels as a "user"-role message carrying
		// tool_result blocks — Anthropic has no separate tool role.

		var content []anthropicContent
		for _, b := range m.Content {
			switch b.Type {
			case message.BlockText:
				if b.Text != "" {
					content = append(content, anthropicContent{Type: "text", Text: b.Text})
				}
			case message.BlockToolUse:
				input := b.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				content = append(content, anthropicContent{Type: "tool_use", ID: b.ID, Name: toWireToolName(b.Name), Input: input})
			case message.BlockToolResult:
				// This is also where a denied approval's synthesized
				// error ("tool call denied: ...", runtime.go's stepTools)
				// reaches the model — as a tool_result with is_error
				// true, not a user-authored message, exactly as
				// Anthropic requires.
				content = append(content, anthropicContent{Type: "tool_result", ToolUseID: b.ToolUseID, Content: b.Content, IsError: b.IsError})
			}
		}
		if len(content) == 0 {
			continue // Anthropic rejects a message with an empty content array
		}
		out = append(out, anthropicMessage{Role: role, Content: content})
	}
	return out
}

func toAnthropicTools(tools []ToolDef) []anthropicTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, anthropicTool{Name: toWireToolName(t.Name), Description: t.Description, InputSchema: t.InputSchema})
	}
	return out
}

func fromAnthropicContent(blocks []anthropicContent) []message.ContentBlock {
	var out []message.ContentBlock
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				out = append(out, message.ContentBlock{Type: message.BlockText, Text: b.Text})
			}
		case "tool_use":
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			out = append(out, message.ContentBlock{Type: message.BlockToolUse, ID: b.ID, Name: fromWireToolName(b.Name), Input: input})
		}
	}
	return out
}

// normalizeAnthropicStopReason folds stop_sequence into end_turn (our
// Response.StopReason only distinguishes end_turn/tool_use/max_tokens);
// anything else passes through unchanged rather than being silently lost.
func normalizeAnthropicStopReason(sr string) string {
	if sr == "stop_sequence" {
		return "end_turn"
	}
	return sr
}

func maxTokensOrDefault(n int) int {
	if n <= 0 {
		return defaultAnthropicMaxTokens
	}
	return n
}

func anthropicErrorMessage(body []byte) string {
	var eb anthropicErrorBody
	if err := json.Unmarshal(body, &eb); err == nil && eb.Error.Message != "" {
		return eb.Error.Message
	}
	return string(body)
}

func (a *Anthropic) buildRequest(r Request, stream bool) anthropicRequest {
	return anthropicRequest{
		Model:       r.Model,
		MaxTokens:   maxTokensOrDefault(r.MaxTokens),
		System:      r.System,
		Messages:    toAnthropicMessages(r.Messages),
		Tools:       toAnthropicTools(r.Tools),
		Temperature: r.Temperature,
		Stream:      stream,
	}
}

func (a *Anthropic) doRequest(ctx context.Context, body anthropicRequest) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.APIKey)
	req.Header.Set("anthropic-version", anthropicAPIVersion)

	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: request failed: %w", err)
	}
	return resp, nil
}

func (a *Anthropic) Complete(ctx context.Context, r Request) (*Response, error) {
	resp, err := a.doRequest(ctx, a.buildRequest(r, false))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic: status %d: %s", resp.StatusCode, anthropicErrorMessage(respBody))
	}

	var ar anthropicResponse
	if err := json.Unmarshal(respBody, &ar); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}

	return &Response{
		Content:    fromAnthropicContent(ar.Content),
		StopReason: normalizeAnthropicStopReason(ar.StopReason),
		Usage:      Usage{InputTokens: ar.Usage.InputTokens, OutputTokens: ar.Usage.OutputTokens},
	}, nil
}

// Stream sends the same request with stream:true and adapts Anthropic's
// SSE event sequence (message_start, content_block_start/delta/stop,
// message_delta, message_stop) onto the pull-based Stream interface.
func (a *Anthropic) Stream(ctx context.Context, r Request) (Stream, error) {
	resp, err := a.doRequest(ctx, a.buildRequest(r, true))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("anthropic: status %d: %s", resp.StatusCode, anthropicErrorMessage(respBody))
	}
	return &anthropicStream{body: resp.Body, reader: newAnthropicSSEReader(resp.Body)}, nil
}

// --- SSE framing ---

type anthropicSSEFrame struct {
	event string
	data  []byte
}

// anthropicSSEReader reads "event: X\ndata: Y\n\n" frames from an
// Anthropic streaming response body.
type anthropicSSEReader struct {
	scanner *bufio.Scanner
}

func newAnthropicSSEReader(body io.Reader) *anthropicSSEReader {
	scanner := bufio.NewScanner(body)
	// A single data: line (e.g. a large input_json_delta) can exceed
	// bufio.Scanner's 64KB default; give it real headroom.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &anthropicSSEReader{scanner: scanner}
}

// next reads the next frame, returning false at EOF or a scan error
// (distinguish the two via err()). A lone blank line with no event seen
// yet is a keep-alive and is skipped rather than returned.
func (r *anthropicSSEReader) next() (anthropicSSEFrame, bool) {
	var frame anthropicSSEFrame
	for r.scanner.Scan() {
		line := r.scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			frame.event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			frame.data = []byte(strings.TrimPrefix(line, "data: "))
		case line == "":
			if frame.event != "" {
				return frame, true
			}
		}
	}
	return frame, false
}

func (r *anthropicSSEReader) err() error { return r.scanner.Err() }

// --- stream assembly ---

// anthropicContentAccum accumulates one content block's streamed pieces:
// text arrives as successive text_delta chunks; a tool_use's input
// arrives as successive raw JSON string fragments (input_json_delta) that
// only parse as a whole once complete, hence the string builder rather
// than incremental JSON decoding.
type anthropicContentAccum struct {
	blockType string
	id        string
	name      string
	text      strings.Builder
	jsonBuf   strings.Builder
}

type anthropicStream struct {
	body   io.ReadCloser
	reader *anthropicSSEReader

	blocks     []*anthropicContentAccum
	curDelta   string
	stopReason string
	usage      Usage

	done bool
	err  error
}

func (s *anthropicStream) Next() bool {
	if s.err != nil || s.done {
		return false
	}
	for {
		frame, ok := s.reader.next()
		if !ok {
			if err := s.reader.err(); err != nil {
				s.err = fmt.Errorf("anthropic: read stream: %w", err)
			} else {
				s.err = fmt.Errorf("anthropic: stream ended without message_stop")
			}
			return false
		}

		s.curDelta = ""
		switch frame.event {
		case "message_start":
			var ev struct {
				Message struct {
					Usage anthropicUsage `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(frame.data, &ev); err != nil {
				s.err = fmt.Errorf("anthropic: decode message_start: %w", err)
				return false
			}
			s.usage.InputTokens = ev.Message.Usage.InputTokens
			return true

		case "content_block_start":
			var ev struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal(frame.data, &ev); err != nil {
				s.err = fmt.Errorf("anthropic: decode content_block_start: %w", err)
				return false
			}
			for len(s.blocks) <= ev.Index {
				s.blocks = append(s.blocks, nil)
			}
			s.blocks[ev.Index] = &anthropicContentAccum{blockType: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			return true

		case "content_block_delta":
			var ev struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(frame.data, &ev); err != nil {
				s.err = fmt.Errorf("anthropic: decode content_block_delta: %w", err)
				return false
			}
			if ev.Index >= len(s.blocks) || s.blocks[ev.Index] == nil {
				s.err = fmt.Errorf("anthropic: content_block_delta for unknown index %d", ev.Index)
				return false
			}
			blk := s.blocks[ev.Index]
			switch ev.Delta.Type {
			case "text_delta":
				blk.text.WriteString(ev.Delta.Text)
				s.curDelta = ev.Delta.Text
			case "input_json_delta":
				blk.jsonBuf.WriteString(ev.Delta.PartialJSON)
			}
			return true

		case "content_block_stop", "ping":
			return true

		case "message_delta":
			var ev struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage anthropicUsage `json:"usage"`
			}
			if err := json.Unmarshal(frame.data, &ev); err != nil {
				s.err = fmt.Errorf("anthropic: decode message_delta: %w", err)
				return false
			}
			s.stopReason = ev.Delta.StopReason
			s.usage.OutputTokens = ev.Usage.OutputTokens
			return true

		case "message_stop":
			s.done = true
			return true

		case "error":
			var ev struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(frame.data, &ev)
			s.err = fmt.Errorf("anthropic: stream error: %s", ev.Error.Message)
			return false

		default:
			// Unrecognized event type: skip rather than fail the whole
			// stream over a field a newer API version added.
			continue
		}
	}
}

func (s *anthropicStream) Delta() string { return s.curDelta }
func (s *anthropicStream) Err() error    { return s.err }

func (s *anthropicStream) Response() (*Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	var blocks []message.ContentBlock
	for _, b := range s.blocks {
		if b == nil {
			continue
		}
		switch b.blockType {
		case "text":
			if t := b.text.String(); t != "" {
				blocks = append(blocks, message.ContentBlock{Type: message.BlockText, Text: t})
			}
		case "tool_use":
			input := json.RawMessage(b.jsonBuf.String())
			if len(input) == 0 || !json.Valid(input) {
				input = json.RawMessage("{}")
			}
			blocks = append(blocks, message.ContentBlock{Type: message.BlockToolUse, ID: b.id, Name: fromWireToolName(b.name), Input: input})
		}
	}
	return &Response{Content: blocks, StopReason: normalizeAnthropicStopReason(s.stopReason), Usage: s.usage}, nil
}

func (s *anthropicStream) Close() error { return s.body.Close() }
