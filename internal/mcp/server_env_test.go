package mcp

import (
	"strings"
	"testing"
)

// TestChildEnvDoesNotLeakTheParentEnvironment proves an MCP subprocess
// doesn't inherit the daemon's full environment — a secret set in the
// parent process must not be visible to a server config's child unless
// explicitly passed via mcp.ServerConfig.Env. connect used to build this
// with os.Environ() directly; a model-driven MCP server that inherited
// it would see every API key and secret the daemon holds.
func TestChildEnvDoesNotLeakTheParentEnvironment(t *testing.T) {
	t.Setenv("AGENTFORGE_TEST_SECRET", "do-not-leak")

	env := childEnv(nil)

	for _, kv := range env {
		if strings.HasPrefix(kv, "AGENTFORGE_TEST_SECRET=") {
			t.Fatalf("child environment leaked the parent's secret: %v", env)
		}
	}
	hasPath := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			hasPath = true
		}
	}
	if !hasPath {
		t.Fatalf("expected PATH to still be passed through, got: %v", env)
	}
}

// TestChildEnvIncludesExplicitOverrides confirms a server's own Env
// entries do reach the child, even though the ambient environment
// doesn't — the escape hatch config.MCPServer.Env exists for.
func TestChildEnvIncludesExplicitOverrides(t *testing.T) {
	env := childEnv(map[string]string{"FOO": "bar"})

	found := false
	for _, kv := range env {
		if kv == "FOO=bar" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected FOO=bar in child environment, got: %v", env)
	}
}
