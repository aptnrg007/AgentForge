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
