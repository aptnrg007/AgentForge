// Package message defines the internal message representation shared by the
// runtime and every provider. It follows Anthropic's content-block shape;
// providers with a flatter format (OpenAI) convert on the way in and out.
package message

import "encoding/json"

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockImage      BlockType = "image" // only ever appears inside ToolResultParts
)

type ContentBlock struct {
	Type BlockType `json:"type"`

	Text string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"` // namespaced: "github.search"
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`

	// ToolResultParts, when non-nil, is a tool_result's real multi-part
	// content (BlockText and/or BlockImage sub-blocks) — set when an MCP
	// tool's result contains something Content's flat string can't
	// represent (e.g. an image). Content is still populated with a text
	// summary in that case (see runtime.summarizeToolResultParts), so
	// every existing caller that only reads Content keeps working
	// unchanged.
	ToolResultParts []ContentBlock `json:"tool_result_parts,omitempty"`

	// Only meaningful on a BlockImage sub-block (i.e. only inside
	// ToolResultParts). Data is base64, matching the wire format every
	// provider's image API expects directly — no re-encoding needed.
	ImageData      string `json:"image_data,omitempty"`
	ImageMediaType string `json:"image_media_type,omitempty"` // e.g. "image/png"

	// Signature carries opaque provider-issued metadata that must be echoed
	// back verbatim when this block is replayed in a later turn. Gemini is
	// the only current user: its thinking models sign functionCall parts
	// and reject history that has lost the signature.
	Signature string `json:"signature,omitempty"`
}

type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// Text returns a Message with a single text content block.
func Text(role Role, text string) Message {
	return Message{Role: role, Content: []ContentBlock{{Type: BlockText, Text: text}}}
}
