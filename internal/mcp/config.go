// Package mcp connects to MCP servers over stdio and exposes their tools as
// runtime.Tool values, namespaced per docs/DESIGN.md section 8 ("server
// github + tool search -> github.search"). One long-lived process is kept
// per unique server config and shared across every namespace that points at
// it; a crashed process is respawned lazily on the next call.
package mcp

// ServerConfig describes how to launch one MCP server over stdio. HTTP
// transport and env var interpolation (${VAR}) are handled by the config
// loader in Phase 3 — by the time a ServerConfig reaches this package, Env
// values are already resolved.
type ServerConfig struct {
	Command []string
	Env     map[string]string
}
