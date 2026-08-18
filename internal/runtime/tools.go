package runtime

import (
	"context"
	"encoding/json"
	"fmt"
)

// NewEchoTool returns a stub tool, registered directly rather than through
// MCP, for exercising the run loop before Phase 2 wires up real MCP servers.
func NewEchoTool() Tool {
	return Tool{
		Name:        "echo",
		Description: "Echoes back the provided message. Useful for testing the tool-calling loop.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			var args struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(input, &args); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			return args.Message, nil
		},
	}
}
