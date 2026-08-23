package mcp

import (
	"encoding/base64"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"agentforge/internal/message"
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

// maxImageBytes caps the raw (decoded) size of an image an MCP tool result
// can contribute — a real screenshot tool has no reason to exceed this,
// and accepting an unbounded blob would bloat both the outbound provider
// request and the messages table row it lands in. Rejected outright, not
// truncated: truncated base64 image bytes don't decode as an image at
// all, unlike the text truncation internal/tools/http.go's
// MaxResponseBytes does.
const maxImageBytes = 5 * 1024 * 1024

// mcpContentToBlocks converts an MCP tool result into content blocks,
// preserving an image as a BlockImage instead of contentToText's
// flatten-everything-to-JSON-text behavior — the model can actually see
// the image this way instead of receiving ~90K tokens of unreadable
// base64.
func mcpContentToBlocks(blocks []mcpsdk.Content) []message.ContentBlock {
	out := make([]message.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch c := b.(type) {
		case *mcpsdk.TextContent:
			out = append(out, message.ContentBlock{Type: message.BlockText, Text: c.Text})
		case *mcpsdk.ImageContent:
			if len(c.Data) > maxImageBytes {
				out = append(out, message.ContentBlock{Type: message.BlockText, Text: fmt.Sprintf(
					"[image omitted: %d bytes exceeds the %d byte limit]", len(c.Data), maxImageBytes)})
				continue
			}
			out = append(out, message.ContentBlock{
				Type:           message.BlockImage,
				ImageData:      base64.StdEncoding.EncodeToString(c.Data),
				ImageMediaType: c.MIMEType,
			})
		default:
			raw, err := b.MarshalJSON()
			if err != nil {
				out = append(out, message.ContentBlock{Type: message.BlockText, Text: fmt.Sprintf("[unrepresentable content: %v]", err)})
				continue
			}
			out = append(out, message.ContentBlock{Type: message.BlockText, Text: string(raw)})
		}
	}
	return out
}

// flattenBlocksToText joins the BlockText parts of blocks, dropping any
// image parts. Used only for the ExecuteRich error path (the MCP server
// flagged IsError): an error needs a plain Go error string, and no MCP
// server has a legitimate reason to put an image in an error result.
func flattenBlocksToText(blocks []message.ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == message.BlockText {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
