package mcp

import (
	"bytes"
	"encoding/base64"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"agentforge/internal/message"
)

// TestMcpContentToBlocksRoundTripsImageBytes is the regression test for
// the vision-support conversion: an *mcpsdk.ImageContent must become a
// BlockImage whose base64 ImageData decodes back to the exact bytes the
// MCP server sent, with MIMEType preserved as ImageMediaType. This is the
// step that replaces contentToText's old behavior of flattening an image
// into unreadable JSON text.
func TestMcpContentToBlocksRoundTripsImageBytes(t *testing.T) {
	raw := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a} // a PNG-like fixture, not a full valid PNG
	blocks := mcpContentToBlocks([]mcpsdk.Content{
		&mcpsdk.ImageContent{Data: raw, MIMEType: "image/png"},
	})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %+v", blocks)
	}
	b := blocks[0]
	if b.Type != message.BlockImage {
		t.Fatalf("expected a BlockImage, got %+v", b)
	}
	if b.ImageMediaType != "image/png" {
		t.Fatalf("expected ImageMediaType image/png, got %q", b.ImageMediaType)
	}
	decoded, err := base64.StdEncoding.DecodeString(b.ImageData)
	if err != nil {
		t.Fatalf("ImageData did not decode as base64: %v", err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Fatalf("expected the image bytes to round-trip, got %x, want %x", decoded, raw)
	}
}

func TestMcpContentToBlocksPassesTextThrough(t *testing.T) {
	blocks := mcpContentToBlocks([]mcpsdk.Content{
		&mcpsdk.TextContent{Text: "hello"},
	})
	if len(blocks) != 1 || blocks[0].Type != message.BlockText || blocks[0].Text != "hello" {
		t.Fatalf("expected a single text block, got %+v", blocks)
	}
}

// TestMcpContentToBlocksRejectsOversizedImage pins the size guard: an
// image over maxImageBytes must come back as a text-only error block, not
// a giant ContentBlock that would bloat both the outbound provider
// request and the messages table row it gets persisted into. Rejected
// outright rather than truncated, since truncated base64 image bytes
// don't decode as an image at all.
func TestMcpContentToBlocksRejectsOversizedImage(t *testing.T) {
	blocks := mcpContentToBlocks([]mcpsdk.Content{
		&mcpsdk.ImageContent{Data: make([]byte, maxImageBytes+1), MIMEType: "image/png"},
	})
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %+v", blocks)
	}
	if blocks[0].Type != message.BlockText {
		t.Fatalf("expected an oversized image to be replaced by a text block, got %+v", blocks[0])
	}
	if blocks[0].ImageData != "" {
		t.Fatalf("expected no image bytes to be carried through for a rejected image, got %+v", blocks[0])
	}
}

func TestFlattenBlocksToTextJoinsOnlyTextParts(t *testing.T) {
	got := flattenBlocksToText([]message.ContentBlock{
		{Type: message.BlockText, Text: "line one"},
		{Type: message.BlockImage, ImageData: "aGVsbG8=", ImageMediaType: "image/png"},
		{Type: message.BlockText, Text: "line two"},
	})
	if got != "line one\nline two" {
		t.Fatalf("expected only the text parts joined, got %q", got)
	}
}
