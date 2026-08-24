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
	defaultGeminiBaseURL = "https://generativelanguage.googleapis.com/v1beta"
	// Gemini's thinking models spend part of maxOutputTokens on an
	// internal reasoning trace before producing any visible text or
	// function call; too low a budget yields an empty response.
	defaultGeminiMaxTokens = 4096
	// geminiLocalIDPrefix marks a tool-call ID this provider synthesized
	// because Gemini didn't send one (observed on non-parallel calls).
	// An ID with this prefix must never be echoed back to Gemini as a
	// functionCall/functionResponse id — Gemini never issued it, and
	// sending it back breaks parallel-call matching, which falls back to
	// name+order when no id is present.
	geminiLocalIDPrefix = "gemini-local-"
)

type Gemini struct {
	APIKey  string
	BaseURL string
	Client  *http.Client
}

func NewGemini(apiKey, baseURL string) *Gemini {
	if baseURL == "" {
		baseURL = defaultGeminiBaseURL
	}
	return &Gemini{APIKey: apiKey, BaseURL: strings.TrimSuffix(baseURL, "/"), Client: newHTTPClient()}
}

func (g *Gemini) Name() string { return "gemini" }

// Capabilities reports no native structured output: Gemini's
// responseSchema is an OpenAPI subset that rejects $schema and
// additionalProperties, so claiming support here would need a
// schema-dialect translation layer. Callers fall back to the engine's
// validate-and-retry path instead, same as a provider with no support at
// all — see TestGeminiIgnoresResponseSchema.
func (g *Gemini) Capabilities() Capabilities {
	return Capabilities{Vision: true, ToolUse: true, StructuredOutput: false}
}

// --- wire format (generateContent / streamGenerateContent) ---

type geminiFunctionCall struct {
	Name string `json:"name"`
	// ID pairs a functionCall with its later functionResponse for
	// parallel-call matching. Optional — see geminiLocalIDPrefix.
	ID   string          `json:"id,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
}

type geminiFunctionResponse struct {
	Name     string          `json:"name"`
	ID       string          `json:"id,omitempty"`
	Response json.RawMessage `json:"response"`
}

// geminiPart covers every part shape used across requests and responses;
// omitempty keeps each wire object down to only the fields its content
// uses, mirroring anthropicContent's approach to the same problem.
type geminiPart struct {
	Text string `json:"text,omitempty"`

	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`

	// ThoughtSignature is a sibling of functionCall (and sometimes text)
	// inside the same part, not a field within functionCall. Gemini's
	// thinking models require it echoed back verbatim on any later turn
	// that replays a functionCall part it was attached to; omitting it
	// is the exact 400 this provider exists to avoid. Only the first
	// part of a set of parallel function calls carries one, so this must
	// stay optional per-part — never synthesize a value.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiGenerationConfig struct {
	Temperature     float64 `json:"temperature,omitempty"`
	MaxOutputTokens int     `json:"maxOutputTokens,omitempty"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent          `json:"systemInstruction,omitempty"`
	Contents          []geminiContent         `json:"contents"`
	Tools             []geminiTool            `json:"tools,omitempty"`
	GenerationConfig  *geminiGenerationConfig `json:"generationConfig,omitempty"`
}

type geminiUsageMetadata struct {
	PromptTokenCount int `json:"promptTokenCount"`
	// CandidatesTokenCount and ThoughtsTokenCount are reported
	// separately (observed 13 vs 138 on a trivial prompt) — both count
	// against generationConfig.maxOutputTokens, so both belong in
	// OutputTokens or usage badly under-reports real consumption.
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	ThoughtsTokenCount   int `json:"thoughtsTokenCount"`
}

type geminiCandidate struct {
	Content geminiContent `json:"content"`
	// FinishReason is "STOP" even when the candidate contains function
	// calls — tool-use must be detected from the presence of a
	// functionCall part, never from this field alone.
	FinishReason string `json:"finishReason"`
}

type geminiResponse struct {
	Candidates    []geminiCandidate   `json:"candidates"`
	UsageMetadata geminiUsageMetadata `json:"usageMetadata"`
}

type geminiErrorBody struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// --- translation ---

// toGeminiContents maps our internal messages onto Gemini's content/part
// shape. message.RoleTool travels as a "user"-role content carrying
// functionResponse parts, exactly as toAnthropicMessages/toOpenAIMessages
// handle tool_result for their respective wire formats.
func toGeminiContents(msgs []message.Message) []geminiContent {
	// functionResponse.name is required by Gemini (a hard 400 if empty),
	// but message.ContentBlock's tool_result only carries ToolUseID —
	// resolve the name by scanning every tool_use block in history once,
	// before the main loop.
	toolNames := map[string]string{}
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == message.BlockToolUse {
				toolNames[b.ID] = b.Name
			}
		}
	}

	var out []geminiContent
	for _, m := range msgs {
		role := "user"
		if m.Role == message.RoleAssistant {
			role = "model"
		}

		var parts []geminiPart
		for _, b := range m.Content {
			switch b.Type {
			case message.BlockText:
				if b.Text == "" {
					continue
				}
				part := geminiPart{Text: b.Text}
				if b.Signature != "" {
					part.ThoughtSignature = b.Signature
				}
				parts = append(parts, part)

			case message.BlockToolUse:
				input := b.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				fc := &geminiFunctionCall{Name: b.Name, Args: input}
				if !strings.HasPrefix(b.ID, geminiLocalIDPrefix) {
					fc.ID = b.ID
				}
				part := geminiPart{FunctionCall: fc}
				if b.Signature != "" {
					part.ThoughtSignature = b.Signature
				}
				parts = append(parts, part)

			case message.BlockToolResult:
				// This is also where a denied approval's synthesized
				// error reaches the model — as a functionResponse whose
				// content is ERROR:-prefixed, matching the OpenAI
				// provider's convention (Gemini's functionResponse has
				// no separate is_error field).
				content := b.Content
				if b.IsError {
					content = "ERROR: " + content
				}
				respJSON, err := json.Marshal(map[string]string{"result": content})
				if err != nil {
					respJSON = json.RawMessage(`{"result":""}`)
				}
				id := b.ToolUseID
				if strings.HasPrefix(id, geminiLocalIDPrefix) {
					id = ""
				}
				parts = append(parts, geminiPart{FunctionResponse: &geminiFunctionResponse{
					Name: toolNames[b.ToolUseID], ID: id, Response: respJSON,
				}})
			}
		}
		if len(parts) == 0 {
			continue // Gemini rejects a content with an empty parts array
		}
		out = append(out, geminiContent{Role: role, Parts: parts})
	}
	return out
}

func toGeminiTools(tools []ToolDef) []geminiTool {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]geminiFunctionDeclaration, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, geminiFunctionDeclaration{Name: t.Name, Description: t.Description, Parameters: sanitizeGeminiSchema(t.InputSchema)})
	}
	return []geminiTool{{FunctionDeclarations: decls}}
}

// geminiUnsupportedSchemaKeywords are standard JSON Schema keywords that
// Gemini's functionDeclarations[].parameters — an OpenAPI 3.0 schema
// subset, not full JSON Schema — rejects outright. Confirmed directly
// against the live API: $schema, additionalProperties (even as a bare
// boolean, not only as a nested schema object), and propertyNames. Every
// other provider passes InputSchema through unmodified; a real MCP
// tool's schema (e.g. zod-to-json-schema's rendering of a
// z.record(z.string(), z.unknown()) field) emits all three, and they can
// appear at any nesting level, not just the top.
var geminiUnsupportedSchemaKeywords = []string{"$schema", "additionalProperties", "propertyNames"}

// sanitizeGeminiSchema strips geminiUnsupportedSchemaKeywords from raw at
// every object level before it is sent as a functionDeclaration's
// parameters. Malformed input is returned unchanged — schema.Compile has
// already validated it upstream, at config load time, so this only ever
// sees well-formed JSON in practice.
func sanitizeGeminiSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	out, err := json.Marshal(stripGeminiUnsupportedSchemaKeywords(v))
	if err != nil {
		return raw
	}
	return out
}

func stripGeminiUnsupportedSchemaKeywords(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for _, key := range geminiUnsupportedSchemaKeywords {
			delete(t, key)
		}
		for k, val := range t {
			t[k] = stripGeminiUnsupportedSchemaKeywords(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = stripGeminiUnsupportedSchemaKeywords(val)
		}
		return t
	default:
		return v
	}
}

// fromGeminiParts turns one candidate's parts into content blocks. Used
// directly by Complete (a full, non-incremental parts list) and, part by
// part, by the stream assembler.
func fromGeminiParts(parts []geminiPart) []message.ContentBlock {
	var out []message.ContentBlock
	for _, p := range parts {
		switch {
		case p.FunctionCall != nil:
			input := p.FunctionCall.Args
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			id := p.FunctionCall.ID
			if id == "" {
				id = geminiLocalIDPrefix + newToolCallID()
			}
			out = append(out, message.ContentBlock{
				Type: message.BlockToolUse, ID: id, Name: p.FunctionCall.Name,
				Input: input, Signature: p.ThoughtSignature,
			})
		case p.Text != "":
			out = append(out, message.ContentBlock{Type: message.BlockText, Text: p.Text, Signature: p.ThoughtSignature})
		}
	}
	return out
}

// normalizeGeminiStopReason maps finishReason onto our
// end_turn/tool_use/max_tokens vocabulary. It does NOT look at parts —
// callers must check for a functionCall part themselves and prefer
// "tool_use" over whatever this returns, since finishReason is "STOP"
// even when the candidate contains function calls.
func normalizeGeminiStopReason(fr string) string {
	switch fr {
	case "MAX_TOKENS":
		return "max_tokens"
	case "STOP":
		return "end_turn"
	default:
		return fr
	}
}

func geminiStopReason(finishReason string, parts []geminiPart) string {
	for _, p := range parts {
		if p.FunctionCall != nil {
			return "tool_use"
		}
	}
	return normalizeGeminiStopReason(finishReason)
}

func geminiMaxTokensOrDefault(n int) int {
	if n <= 0 {
		return defaultGeminiMaxTokens
	}
	return n
}

func geminiErrorMessage(body []byte) string {
	var eb geminiErrorBody
	if err := json.Unmarshal(body, &eb); err == nil && eb.Error.Message != "" {
		return eb.Error.Message
	}
	return string(body)
}

func (g *Gemini) buildRequest(r Request) geminiRequest {
	req := geminiRequest{
		Contents: toGeminiContents(r.Messages),
		Tools:    toGeminiTools(r.Tools),
		GenerationConfig: &geminiGenerationConfig{
			Temperature:     r.Temperature,
			MaxOutputTokens: geminiMaxTokensOrDefault(r.MaxTokens),
		},
	}
	if r.System != "" {
		req.SystemInstruction = &geminiContent{Parts: []geminiPart{{Text: r.System}}}
	}
	return req
}

func (g *Gemini) endpoint(model, method string) string {
	return fmt.Sprintf("%s/models/%s:%s", g.BaseURL, model, method)
}

func (g *Gemini) doRequest(ctx context.Context, model, method string, body geminiRequest, sse bool) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("gemini: marshal request: %w", err)
	}

	url := g.endpoint(model, method)
	if sse {
		url += "?alt=sse"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("gemini: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", g.APIKey) // Gemini auth: a header, not a bearer token

	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: request failed: %w", err)
	}
	return resp, nil
}

func (g *Gemini) Complete(ctx context.Context, r Request) (*Response, error) {
	resp, err := g.doRequest(ctx, r.Model, "generateContent", g.buildRequest(r), false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("gemini: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini: status %d: %s", resp.StatusCode, geminiErrorMessage(respBody))
	}

	var gr geminiResponse
	if err := json.Unmarshal(respBody, &gr); err != nil {
		return nil, fmt.Errorf("gemini: decode response: %w", err)
	}
	if len(gr.Candidates) == 0 {
		return nil, fmt.Errorf("gemini: response has no candidates")
	}
	cand := gr.Candidates[0]

	return &Response{
		Content:    fromGeminiParts(cand.Content.Parts),
		StopReason: geminiStopReason(cand.FinishReason, cand.Content.Parts),
		Usage: Usage{
			InputTokens:  gr.UsageMetadata.PromptTokenCount,
			OutputTokens: gr.UsageMetadata.CandidatesTokenCount + gr.UsageMetadata.ThoughtsTokenCount,
		},
	}, nil
}

// Stream sends the same request against streamGenerateContent?alt=sse and
// adapts Gemini's frame sequence onto the pull-based Stream interface.
// Unlike Anthropic (event: + message_stop) or OpenAI (data: [DONE]),
// Gemini's SSE stream carries no event: field and no terminal sentinel —
// it simply ends at EOF, so geminiSSEReader and geminiStream both treat a
// clean EOF as normal completion rather than an error.
func (g *Gemini) Stream(ctx context.Context, r Request) (Stream, error) {
	resp, err := g.doRequest(ctx, r.Model, "streamGenerateContent", g.buildRequest(r), true)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("gemini: status %d: %s", resp.StatusCode, geminiErrorMessage(respBody))
	}
	return &geminiStream{body: resp.Body, reader: newGeminiSSEReader(resp.Body)}, nil
}

// --- SSE framing ---

// geminiSSEReader reads bare "data: <json>" lines from a
// streamGenerateContent response body. There is no event: field to key
// off (unlike anthropicSSEReader) and no blank-line frame terminator
// (unlike either anthropicSSEReader or openAISSEReader's [DONE] line) —
// every data: line is a complete, self-contained GenerateContentResponse.
type geminiSSEReader struct {
	scanner *bufio.Scanner
}

func newGeminiSSEReader(body io.Reader) *geminiSSEReader {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	return &geminiSSEReader{scanner: scanner}
}

func (r *geminiSSEReader) next() ([]byte, bool) {
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			return []byte(data), true
		}
	}
	return nil, false
}

func (r *geminiSSEReader) err() error { return r.scanner.Err() }

// --- stream assembly ---

type geminiStream struct {
	body   io.ReadCloser
	reader *geminiSSEReader

	text          strings.Builder
	textSignature string
	toolCalls     []message.ContentBlock

	rawFinishReason string
	usage           Usage

	curDelta string
	done     bool
	err      error
}

func (s *geminiStream) Next() bool {
	if s.err != nil || s.done {
		return false
	}

	data, ok := s.reader.next()
	if !ok {
		// A clean EOF (reader.err() == nil) is normal completion for
		// Gemini — there is no terminal frame to wait for.
		if err := s.reader.err(); err != nil {
			s.err = fmt.Errorf("gemini: read stream: %w", err)
		} else {
			s.done = true
		}
		return false
	}

	s.curDelta = ""
	var chunk geminiResponse
	if err := json.Unmarshal(data, &chunk); err != nil {
		s.err = fmt.Errorf("gemini: decode stream chunk: %w", err)
		return false
	}
	if chunk.UsageMetadata != (geminiUsageMetadata{}) {
		s.usage = Usage{
			InputTokens:  chunk.UsageMetadata.PromptTokenCount,
			OutputTokens: chunk.UsageMetadata.CandidatesTokenCount + chunk.UsageMetadata.ThoughtsTokenCount,
		}
	}
	if len(chunk.Candidates) == 0 {
		return true
	}

	cand := chunk.Candidates[0]
	if cand.FinishReason != "" {
		s.rawFinishReason = cand.FinishReason
	}
	for _, p := range cand.Content.Parts {
		switch {
		case p.FunctionCall != nil:
			input := p.FunctionCall.Args
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			id := p.FunctionCall.ID
			if id == "" {
				id = geminiLocalIDPrefix + newToolCallID()
			}
			s.toolCalls = append(s.toolCalls, message.ContentBlock{
				Type: message.BlockToolUse, ID: id, Name: p.FunctionCall.Name,
				Input: input, Signature: p.ThoughtSignature,
			})
		case p.Text != "":
			s.text.WriteString(p.Text)
			s.curDelta = p.Text
			if p.ThoughtSignature != "" {
				s.textSignature = p.ThoughtSignature
			}
		default:
			// An empty-text part can still carry a trailing signature
			// (observed) — attach it to the accumulated text block.
			if p.ThoughtSignature != "" {
				s.textSignature = p.ThoughtSignature
			}
		}
	}
	return true
}

func (s *geminiStream) Delta() string { return s.curDelta }
func (s *geminiStream) Err() error    { return s.err }

func (s *geminiStream) Response() (*Response, error) {
	if s.err != nil {
		return nil, s.err
	}
	var blocks []message.ContentBlock
	if t := s.text.String(); t != "" {
		blocks = append(blocks, message.ContentBlock{Type: message.BlockText, Text: t, Signature: s.textSignature})
	}
	blocks = append(blocks, s.toolCalls...)

	stopReason := normalizeGeminiStopReason(s.rawFinishReason)
	if len(s.toolCalls) > 0 {
		stopReason = "tool_use"
	}
	return &Response{Content: blocks, StopReason: stopReason, Usage: s.usage}, nil
}

func (s *geminiStream) Close() error { return s.body.Close() }
