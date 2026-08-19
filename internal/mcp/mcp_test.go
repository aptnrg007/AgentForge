package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"agentforge/internal/message"
	"agentforge/internal/provider"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

// These tests exercise the real, publicly published
// @modelcontextprotocol/server-everything MCP server over stdio — a
// reference server built for exactly this kind of exercise, requiring no
// credentials. They're skipped if npx isn't on PATH.

func requireNpx(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available; skipping test against the real MCP server")
	}
}

func everythingServerConfig() ServerConfig {
	return ServerConfig{Command: []string{"npx", "-y", "@modelcontextprotocol/server-everything"}}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func toolNames(tools []runtime.Tool) []string {
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	return names
}

func containsName(tools []runtime.Tool, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func findTool(tools []runtime.Tool, name string) (runtime.Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return runtime.Tool{}, false
}

func TestListToolsFromRealServer(t *testing.T) {
	requireNpx(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	reg := NewRegistry(discardLogger())
	defer reg.Close()

	tools, err := reg.Tools(ctx, "everything", everythingServerConfig())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("expected at least one tool from the everything server")
	}
	if !containsName(tools, "everything.echo") {
		t.Fatalf("expected namespaced tool everything.echo, got %v", toolNames(tools))
	}
	for _, tool := range tools {
		if tool.Description == "" {
			t.Errorf("tool %s has no description", tool.Name)
		}
	}
}

// scriptedProvider replays fixed responses, standing in for a real LLM so
// the test exercises only the MCP call path deterministically.
type scriptedProvider struct {
	responses []*provider.Response
	calls     int
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	if p.calls >= len(p.responses) {
		return nil, fmt.Errorf("scripted provider: no more responses")
	}
	resp := p.responses[p.calls]
	p.calls++
	return resp, nil
}

func (p *scriptedProvider) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	resp, err := p.Complete(ctx, r)
	if err != nil {
		return nil, err
	}
	return provider.NewResponseStream(resp), nil
}

func (p *scriptedProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

func TestRuntimeCallsRealMCPTool(t *testing.T) {
	requireNpx(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	reg := NewRegistry(discardLogger())
	defer reg.Close()

	tools, err := reg.Tools(ctx, "everything", everythingServerConfig())
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	fp := &scriptedProvider{responses: []*provider.Response{
		{
			Content: []message.ContentBlock{{
				Type: message.BlockToolUse, ID: "call_1", Name: "everything.echo",
				Input: json.RawMessage(`{"message":"hello from agentforge"}`),
			}},
			StopReason: "tool_use",
		},
		{
			Content:    []message.ContentBlock{{Type: message.BlockText, Text: "the tool replied"}},
			StopReason: "end_turn",
		},
	}}

	eng := runtime.NewEngine(st, fp, runtime.Config{AgentName: "mcp-test", Model: "test-model", MaxTurns: 10})
	for _, tool := range tools {
		eng.RegisterTool(tool)
	}

	runID := "run-mcp"
	if err := eng.NewRun(ctx, runID, "please echo hello from agentforge"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	var state runtime.State
	for i := 0; i < 10; i++ {
		state, err = eng.Step(ctx, runID)
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if state == runtime.StateCompleted || state == runtime.StateFailed {
			break
		}
	}
	if state != runtime.StateCompleted {
		run, _ := st.GetRun(ctx, runID)
		t.Fatalf("expected completed, got %s (error=%v)", state, run.Error)
	}

	msgs, err := st.ListMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var gotResult string
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == message.BlockToolResult {
				gotResult = b.Content
			}
		}
	}
	if gotResult != "Echo: hello from agentforge" {
		t.Fatalf("expected real MCP echo result, got %q", gotResult)
	}
}

func TestReconnectAfterProcessKilled(t *testing.T) {
	requireNpx(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	reg := NewRegistry(discardLogger())
	defer reg.Close()

	cfg := everythingServerConfig()
	tools, err := reg.Tools(ctx, "everything", cfg)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	echo, ok := findTool(tools, "everything.echo")
	if !ok {
		t.Fatal("everything.echo tool not found")
	}

	if _, err := echo.Execute(ctx, json.RawMessage(`{"message":"before kill"}`)); err != nil {
		t.Fatalf("initial call failed: %v", err)
	}

	srv := reg.getOrCreate(cfg)
	srv.mu.Lock()
	proc := srv.cmd.Process
	srv.mu.Unlock()
	if proc == nil {
		t.Fatal("expected a running subprocess")
	}
	// Kill the whole process group, not just the wrapper (npx) PID: npx
	// forks the actual server, and killing only npx would leave that child
	// alive holding the stdout pipe open, so we'd never observe EOF.
	if err := syscall.Kill(-proc.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill process group: %v", err)
	}

	// Give the session's Wait() watcher goroutine a moment to notice the
	// process died and clear the cached session.
	deadline := time.Now().Add(10 * time.Second)
	for {
		srv.mu.Lock()
		dead := srv.session == nil
		srv.mu.Unlock()
		if dead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session was not cleared after the process was killed")
		}
		time.Sleep(50 * time.Millisecond)
	}

	out, err := echo.Execute(ctx, json.RawMessage(`{"message":"after kill"}`))
	if err != nil {
		t.Fatalf("expected reconnect and successful call after kill, got error: %v", err)
	}
	if out != "Echo: after kill" {
		t.Fatalf("unexpected result after reconnect: %q", out)
	}
}

func TestTwoNamespacesShareServerWithoutCollision(t *testing.T) {
	requireNpx(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	reg := NewRegistry(discardLogger())
	defer reg.Close()

	cfg := everythingServerConfig()
	toolsA, err := reg.Tools(ctx, "alpha", cfg)
	if err != nil {
		t.Fatalf("Tools(alpha): %v", err)
	}
	toolsB, err := reg.Tools(ctx, "beta", cfg)
	if err != nil {
		t.Fatalf("Tools(beta): %v", err)
	}

	if !containsName(toolsA, "alpha.echo") {
		t.Fatalf("expected alpha.echo, got %v", toolNames(toolsA))
	}
	if !containsName(toolsB, "beta.echo") {
		t.Fatalf("expected beta.echo, got %v", toolNames(toolsB))
	}

	reg.mu.Lock()
	serverCount := len(reg.servers)
	reg.mu.Unlock()
	if serverCount != 1 {
		t.Fatalf("expected one shared server process for identical configs, got %d", serverCount)
	}

	echoA, _ := findTool(toolsA, "alpha.echo")
	echoB, _ := findTool(toolsB, "beta.echo")

	outA, err := echoA.Execute(ctx, json.RawMessage(`{"message":"from alpha"}`))
	if err != nil {
		t.Fatalf("echoA: %v", err)
	}
	outB, err := echoB.Execute(ctx, json.RawMessage(`{"message":"from beta"}`))
	if err != nil {
		t.Fatalf("echoB: %v", err)
	}
	if outA != "Echo: from alpha" || outB != "Echo: from beta" {
		t.Fatalf("unexpected results: outA=%q outB=%q", outA, outB)
	}
}
