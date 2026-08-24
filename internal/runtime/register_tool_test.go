package runtime

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestRegisterToolRejectsNameCollision confirms a second RegisterTool
// call for a name that's already registered errors instead of silently
// overwriting the first — e.g. a tool_definitions entry named
// "github.foo" used to be able to silently shadow MCP server "github"'s
// "foo" tool with no signal that happened.
func TestRegisterToolRejectsNameCollision(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	eng := NewEngine(st, nil, Config{AgentName: "a", Model: "m"})

	if err := eng.RegisterTool(NewEchoTool()); err != nil {
		t.Fatalf("first RegisterTool: %v", err)
	}

	second := NewEchoTool()
	second.Description = "a completely different implementation"
	err := eng.RegisterTool(second)
	if err == nil {
		t.Fatal("expected a second RegisterTool with the same name to error")
	}
	if !strings.Contains(err.Error(), `"echo"`) {
		t.Fatalf("error = %v, want it to name the colliding tool", err)
	}

	// And the first registration must still be the one in effect.
	if eng.tools["echo"].Description != NewEchoTool().Description {
		t.Fatal("a failed second RegisterTool must not have overwritten the first")
	}
}
