package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	initialBackoff = 200 * time.Millisecond
	maxBackoff     = 30 * time.Second
)

// server supervises one long-lived MCP subprocess for a given ServerConfig.
// It connects lazily on first use, caches tool discovery, and reconnects
// (with backoff) if the process dies mid-session.
type server struct {
	cfg    ServerConfig
	client *mcpsdk.Client
	logger *slog.Logger

	mu        sync.Mutex
	session   *mcpsdk.ClientSession
	cmd       *exec.Cmd
	tools     []*mcpsdk.Tool
	attempts  int
	nextRetry time.Time
}

func newServer(cfg ServerConfig, logger *slog.Logger) *server {
	return &server{
		cfg:    cfg,
		client: mcpsdk.NewClient(&mcpsdk.Implementation{Name: "agentforge", Version: "0.1.0"}, nil),
		logger: logger,
	}
}

// ensureSession returns a live session, reconnecting if the previous one
// died. Repeated failures back off exponentially up to maxBackoff so a
// genuinely broken server doesn't get hammered with respawns.
func (s *server) ensureSession(ctx context.Context) (*mcpsdk.ClientSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.session != nil {
		return s.session, nil
	}

	if now := time.Now(); now.Before(s.nextRetry) {
		return nil, fmt.Errorf("mcp: server unavailable, retrying in %s", s.nextRetry.Sub(now).Round(time.Millisecond))
	}

	session, err := s.connect(ctx)
	if err != nil {
		s.attempts++
		backoff := min(initialBackoff*time.Duration(1<<min(s.attempts-1, 10)), maxBackoff)
		s.nextRetry = time.Now().Add(backoff)
		s.logger.Error("mcp: connect failed", "attempts", s.attempts, "retry_in", backoff, "error", err)
		return nil, fmt.Errorf("mcp: connect: %w", err)
	}

	s.attempts = 0
	s.session = session
	s.logger.Info("mcp: connected", "command", s.cfg.Command)

	// Watch for the process/session dying so the next call reconnects
	// instead of using a dead handle.
	go func(dead *mcpsdk.ClientSession) {
		waitErr := dead.Wait()
		s.mu.Lock()
		if s.session == dead {
			s.logger.Warn("mcp: session ended, will reconnect on next use", "error", waitErr)
			s.session = nil
		}
		s.mu.Unlock()
	}(session)

	return session, nil
}

func (s *server) connect(ctx context.Context) (*mcpsdk.ClientSession, error) {
	if len(s.cfg.Command) == 0 {
		return nil, fmt.Errorf("server config has no command")
	}
	// Deliberately not exec.CommandContext: the subprocess must outlive any
	// single request's context. The registry owns its lifetime via Close.
	cmd := exec.Command(s.cfg.Command[0], s.cfg.Command[1:]...)
	cmd.Env = os.Environ()
	for k, v := range s.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Many MCP servers are launched via a wrapper (npx, uvx, ...) that forks
	// the real server and can exit without it. Putting the process in its
	// own group lets a hard kill take down the whole tree, not just the
	// wrapper — otherwise an orphaned grandchild keeps the stdout pipe open
	// and we never observe EOF to trigger a reconnect.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	transport := &mcpsdk.CommandTransport{Command: cmd}
	session, err := s.client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	s.cmd = cmd
	return session, nil
}

// discoverTools lists and caches the server's tools, connecting first if
// needed.
func (s *server) discoverTools(ctx context.Context) ([]*mcpsdk.Tool, error) {
	s.mu.Lock()
	if s.tools != nil {
		tools := s.tools
		s.mu.Unlock()
		return tools, nil
	}
	s.mu.Unlock()

	session, err := s.ensureSession(ctx)
	if err != nil {
		return nil, err
	}

	var all []*mcpsdk.Tool
	cursor := ""
	for {
		res, err := session.ListTools(ctx, &mcpsdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("mcp: list tools: %w", err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}

	s.mu.Lock()
	s.tools = all
	s.mu.Unlock()
	return all, nil
}

// callTool invokes a bare (non-namespaced) tool name. The returned bool
// reports whether the MCP server flagged the result as a tool-level error
// (as opposed to a transport/protocol error, which comes back as err).
func (s *server) callTool(ctx context.Context, name string, args json.RawMessage) (string, bool, error) {
	session, err := s.ensureSession(ctx)
	if err != nil {
		return "", false, err
	}

	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		s.mu.Lock()
		if s.session == session {
			s.session = nil
		}
		s.mu.Unlock()
		return "", false, fmt.Errorf("mcp: call tool %q: %w", name, err)
	}

	return contentToText(res.Content), res.IsError, nil
}

func (s *server) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.session == nil {
		return nil
	}
	err := s.session.Close()
	s.session = nil
	return err
}
