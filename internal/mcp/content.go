package mcp

import (
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// contentToText flattens an MCP tool result into the plain string our
// internal tool_result content block expects. Text content passes through
// verbatim; anything else (images, resource links, ...) is represented by
// its raw JSON so nothing is silently dropped.
func contentToText(blocks []mcpsdk.Content) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if tc, ok := b.(*mcpsdk.TextContent); ok {
			parts = append(parts, tc.Text)
			continue
		}
		raw, err := b.MarshalJSON()
		if err != nil {
			parts = append(parts, fmt.Sprintf("[unrepresentable content: %v]", err))
			continue
		}
		parts = append(parts, string(raw))
	}
	return strings.Join(parts, "\n")
}
