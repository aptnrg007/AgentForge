package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"agentforge/internal/message"
	"agentforge/internal/runtime"
)

// Registry supervises every MCP server a process talks to, deduplicating
// identical configs to one shared subprocess (PLAN.md section 8: "one
// long-lived process per unique server config, shared across agents and
// runs").
type Registry struct {
	logger *slog.Logger

	mu      sync.Mutex
	servers map[string]*server
}

func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.Default()
	}
	return &Registry{logger: logger, servers: map[string]*server{}}
}

// Tools connects (lazily) to the server described by cfg, discovers its
// tools, and returns them as runtime.Tool values namespaced
// "<namespace>.<tool>" — e.g. namespace "github" + tool "search" becomes
// "github.search". Two registrations with different namespaces but an
// identical cfg share one underlying process without their tool names
// colliding.
func (r *Registry) Tools(ctx context.Context, namespace string, cfg ServerConfig) ([]runtime.Tool, error) {
	srv := r.getOrCreate(cfg)

	mcpTools, err := srv.discoverTools(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]runtime.Tool, 0, len(mcpTools))
	for _, t := range mcpTools {
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			schema = json.RawMessage(`{}`)
		}
		bareName := t.Name
		readOnly := t.Annotations != nil && t.Annotations.ReadOnlyHint
		out = append(out, runtime.Tool{
			Name:         namespace + "." + bareName,
			Description:  t.Description,
			InputSchema:  schema,
			ReadOnlyHint: readOnly,
			Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
				text, isError, err := srv.callTool(ctx, bareName, input)
				if err != nil {
					return "", err
				}
				if isError {
					return "", fmt.Errorf("%s", text)
				}
				return text, nil
			},
			// ExecuteRich is preferred by stepTools whenever it's set
			// (see runtime.Tool.ExecuteRich) — Execute above stays as a
			// flat-text fallback for any other caller (e.g.
			// TestReconnectAfterProcessKilled calls .Execute directly).
			ExecuteRich: func(ctx context.Context, input json.RawMessage) ([]message.ContentBlock, error) {
				res, err := srv.callToolRaw(ctx, bareName, input)
				if err != nil {
					return nil, err
				}
				blocks := mcpContentToBlocks(res.Content)
				if res.IsError {
					return nil, fmt.Errorf("%s", flattenBlocksToText(blocks))
				}
				return blocks, nil
			},
		})
	}
	return out, nil
}

// Close gracefully shuts down every managed server process.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var firstErr error
	for _, s := range r.servers {
		if err := s.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *Registry) getOrCreate(cfg ServerConfig) *server {
	key := configKey(cfg)

	r.mu.Lock()
	defer r.mu.Unlock()

	if s, ok := r.servers[key]; ok {
		return s
	}
	s := newServer(cfg, r.logger)
	r.servers[key] = s
	return s
}

func configKey(cfg ServerConfig) string {
	h := sha256.New()
	for _, c := range cfg.Command {
		h.Write([]byte(c))
		h.Write([]byte{0})
	}

	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{'='})
		h.Write([]byte(cfg.Env[k]))
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil))
}
