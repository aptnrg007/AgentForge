package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"agentforge/internal/message"
	"agentforge/internal/provider"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

// These tests drive chatModel.Update directly with synthetic tea.Msg
// values, the same way the bubbletea runtime would, but without a real
// terminal — bubbletea models are plain value types with no hidden global
// state, so this is a faithful and fast way to exercise the REPL's control
// flow (docs/DESIGN.md section 11's fixture-driven testing approach applied
// to a TUI instead of the run loop).

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

func toolUseResponse(id, name, input string) *provider.Response {
	return &provider.Response{
		Content:    []message.ContentBlock{{Type: message.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(input)}},
		StopReason: "tool_use",
	}
}

func textResponse(text string) *provider.Response {
	return &provider.Response{
		Content:    []message.ContentBlock{{Type: message.BlockText, Text: text}},
		StopReason: "end_turn",
	}
}

func pressRune(m chatModel, r rune) (chatModel, tea.Cmd) {
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	return next.(chatModel), cmd
}

func pressKey(m chatModel, kt tea.KeyType) (chatModel, tea.Cmd) {
	next, cmd := m.Update(tea.KeyMsg{Type: kt})
	return next.(chatModel), cmd
}

func containsSubstring(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func typeText(m chatModel, s string) chatModel {
	for _, r := range s {
		m, _ = pressRune(m, r)
	}
	return m
}

// drive executes cmd, feeds its message back into Update, and repeats for
// whatever command that produces — mirroring what the bubbletea runtime
// loop does — until a step produces no further command.
func drive(t *testing.T, m chatModel, cmd tea.Cmd) chatModel {
	t.Helper()
	for i := 0; cmd != nil; i++ {
		if i > 50 {
			t.Fatal("drive: too many steps, likely an infinite command loop")
		}
		msg := cmd()
		next, nextCmd := m.Update(msg)
		m = next.(chatModel)
		cmd = nextCmd
	}
	return m
}

func TestChatRequireApprovalPausesForYesNo(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	fp := &scriptedProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{"x":1}`),
		textResponse("done"),
	}}
	eng := runtime.NewEngine(st, fp, runtime.Config{
		AgentName: "chat-test", Model: "test-model", MaxTurns: 10,
		Approvals: runtime.ApprovalPolicy{Require: []string{"danger.tool"}},
	})
	var executed bool
	eng.RegisterTool(runtime.Tool{
		Name: "danger.tool", InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			executed = true
			return "ok", nil
		},
	})

	m := newChatModel(ctx, eng, st, "chat-test")
	m = typeText(m, "hello")
	m, cmd := pressKey(m, tea.KeyEnter)
	if m.mode != modeStepping {
		t.Fatalf("expected modeStepping right after submit, got %v", m.mode)
	}
	m = drive(t, m, cmd)

	if m.mode != modeApproval {
		t.Fatalf("expected modeApproval, got %v (err=%v)", m.mode, m.err)
	}
	if len(m.pending) != 1 || m.pending[0].ToolName != "danger.tool" {
		t.Fatalf("expected one pending call for danger.tool, got %+v", m.pending)
	}
	if executed {
		t.Fatal("tool must not run before a decision is recorded")
	}
	if !containsSubstring(m.transcript, "danger.tool") {
		t.Fatalf("expected the transcript to show the pending call before it's decided, got %+v", m.transcript)
	}

	m, cmd = pressRune(m, 'y')
	m = drive(t, m, cmd)

	if m.mode != modeInput {
		t.Fatalf("expected modeInput after completion, got %v (err=%v)", m.mode, m.err)
	}
	if !executed {
		t.Fatal("expected the approved tool to have executed")
	}
}

func TestChatDenyDoesNotExecuteAndAgentContinues(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	fp := &scriptedProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{"x":1}`),
		textResponse("no problem, trying something else"),
	}}
	eng := runtime.NewEngine(st, fp, runtime.Config{
		AgentName: "chat-test", Model: "test-model", MaxTurns: 10,
		Approvals: runtime.ApprovalPolicy{Require: []string{"danger.tool"}},
	})
	var executed bool
	eng.RegisterTool(runtime.Tool{
		Name: "danger.tool", InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			executed = true
			return "ok", nil
		},
	})

	m := newChatModel(ctx, eng, st, "chat-test")
	m = typeText(m, "hello")
	m, cmd := pressKey(m, tea.KeyEnter)
	m = drive(t, m, cmd)
	if m.mode != modeApproval {
		t.Fatalf("expected modeApproval, got %v (err=%v)", m.mode, m.err)
	}

	m, cmd = pressRune(m, 'n')
	m = drive(t, m, cmd)

	if m.mode != modeInput {
		t.Fatalf("expected modeInput after completion, got %v (err=%v)", m.mode, m.err)
	}
	if executed {
		t.Fatal("denied tool must never execute")
	}
}

func TestChatTranscriptShowsRunID(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	fp := &scriptedProvider{responses: []*provider.Response{textResponse("hi")}}
	eng := runtime.NewEngine(st, fp, runtime.Config{AgentName: "chat-test", Model: "test-model", MaxTurns: 10})

	m := newChatModel(ctx, eng, st, "chat-test")
	m = typeText(m, "hello")
	m, cmd := pressKey(m, tea.KeyEnter)
	m = drive(t, m, cmd)

	if m.runID == "" {
		t.Fatal("expected a run ID to have been generated")
	}
	if !containsSubstring(m.transcript, "run: "+m.runID) {
		t.Fatalf("expected the transcript to show the run ID %q, got %+v", m.runID, m.transcript)
	}
}

func TestChatEditArgsExecutesEditedVersion(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	fp := &scriptedProvider{responses: []*provider.Response{
		toolUseResponse("call_1", "danger.tool", `{"x":"original"}`),
		textResponse("done"),
	}}
	eng := runtime.NewEngine(st, fp, runtime.Config{
		AgentName: "chat-test", Model: "test-model", MaxTurns: 10,
		Approvals: runtime.ApprovalPolicy{Require: []string{"danger.tool"}},
	})
	var capturedArgs string
	eng.RegisterTool(runtime.Tool{
		Name: "danger.tool", InputSchema: json.RawMessage(`{}`),
		Execute: func(ctx context.Context, input json.RawMessage) (string, error) {
			capturedArgs = string(input)
			return "ok", nil
		},
	})

	m := newChatModel(ctx, eng, st, "chat-test")
	m = typeText(m, "hello")
	m, cmd := pressKey(m, tea.KeyEnter)
	m = drive(t, m, cmd)
	if m.mode != modeApproval {
		t.Fatalf("expected modeApproval, got %v (err=%v)", m.mode, m.err)
	}

	m, cmd = pressRune(m, 'e')
	if cmd != nil {
		m = drive(t, m, cmd)
	}
	if m.mode != modeEditInput {
		t.Fatalf("expected modeEditInput, got %v", m.mode)
	}
	if got := m.editInput.Value(); got != `{"x":"original"}` {
		t.Fatalf("expected edit input prefilled with original args, got %q", got)
	}

	m.editInput.SetValue(`{"x":"edited"}`)
	m, cmd = pressKey(m, tea.KeyEnter)
	if m.mode != modeStepping {
		t.Fatalf("expected modeStepping after confirming the edit, got %v (err=%v)", m.mode, m.err)
	}
	m = drive(t, m, cmd)

	if m.mode != modeInput {
		t.Fatalf("expected modeInput after completion, got %v (err=%v)", m.mode, m.err)
	}
	if capturedArgs != `{"x":"edited"}` {
		t.Fatalf("expected the tool to execute with the edited args, got %q", capturedArgs)
	}

	toolCalls, err := st.ListToolCalls(ctx, m.runID)
	if err != nil {
		t.Fatalf("ListToolCalls: %v", err)
	}
	if len(toolCalls) != 1 || toolCalls[0].ArgsJSON != `{"x":"edited"}` {
		t.Fatalf("expected the persisted call to reflect the edit, got %+v", toolCalls)
	}
}
