package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"agentforge/internal/message"
	"agentforge/internal/runtime"
	"agentforge/internal/store"
)

// chatMode tracks what the REPL is waiting on. It's distinct from
// runtime.State: the engine only knows about the run, not about whether a
// human is mid-edit of a pending call's arguments.
type chatMode int

const (
	modeInput chatMode = iota
	modeStepping
	modeApproval
	modeEditInput
	modeQuit
)

type stepMsg struct {
	state runtime.State
	err   error
}

type pendingLoadedMsg struct {
	pending []store.ToolCall
	err     error
}

type chatModel struct {
	ctx       context.Context
	eng       *runtime.Engine
	st        *store.Store
	agentName string

	runID  string
	hasRun bool
	// lastState is the run's state as of the most recent stop point
	// (StateCompleted/StateFailed/StateInterrupted/StateAwaitingApproval —
	// see handleStep). Unlike those, updateInput needs to know it too: a
	// StateInterrupted run resumes by stepping again with no new user
	// message, unlike a terminal run's next Enter, which starts a new
	// turn via ContinueRun.
	lastState runtime.State

	transcript    []string
	shownMsgCount int

	input     textinput.Model
	editInput textinput.Model

	mode       chatMode
	pending    []store.ToolCall
	pendingIdx int

	err error
}

func newChatModel(ctx context.Context, eng *runtime.Engine, st *store.Store, agentName string) chatModel {
	in := textinput.New()
	in.Placeholder = "message"
	in.Prompt = "> "
	in.Focus()

	edit := textinput.New()
	edit.Prompt = "args> "
	edit.Width = 100

	return chatModel{
		ctx:        ctx,
		eng:        eng,
		st:         st,
		agentName:  agentName,
		input:      in,
		editInput:  edit,
		mode:       modeInput,
		transcript: []string{fmt.Sprintf("chatting with %s (ctrl+c to quit)", agentName)},
	}
}

func (m chatModel) Init() tea.Cmd {
	return textinput.Blink
}

func (m chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
		switch m.mode {
		case modeInput:
			return m.updateInput(msg)
		case modeApproval:
			return m.updateApproval(msg)
		case modeEditInput:
			return m.updateEditInput(msg)
		}
	case stepMsg:
		return m.handleStep(msg)
	case pendingLoadedMsg:
		return m.handlePendingLoaded(msg)
	}
	return m, nil
}

func (m chatModel) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyEnter {
		// An interrupted run resumes on the next Enter regardless of what
		// (if anything) is typed — retrying the same pending model call,
		// not starting a new turn, so ContinueRun (which requires a
		// terminal run) never enters into it. Any typed text is left in
		// the input box rather than discarded, in case it was meant for
		// after the retry.
		if m.hasRun && m.lastState == runtime.StateInterrupted {
			m.transcript = append(m.transcript, "retrying...")
			m.mode = modeStepping
			return m, m.stepCmd()
		}

		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return m, nil
		}
		m.transcript = append(m.transcript, "you: "+text)
		m.input.SetValue("")

		var err error
		if !m.hasRun {
			m.runID = newRunID()
			m.transcript = append(m.transcript, "run: "+m.runID)
			err = m.eng.NewRun(m.ctx, m.runID, text)
			m.hasRun = true
		} else {
			err = m.eng.ContinueRun(m.ctx, m.runID, text)
		}
		if err != nil {
			m.err = err
			m.mode = modeQuit
			return m, tea.Quit
		}
		m.mode = modeStepping
		return m, m.stepCmd()
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m chatModel) stepCmd() tea.Cmd {
	ctx, eng, runID := m.ctx, m.eng, m.runID
	return func() tea.Msg {
		state, err := eng.Step(ctx, runID)
		return stepMsg{state: state, err: err}
	}
}

func (m chatModel) loadPendingCmd() tea.Cmd {
	ctx, st, runID := m.ctx, m.st, m.runID
	return func() tea.Msg {
		pending, err := st.ListPendingApprovals(ctx, runID)
		return pendingLoadedMsg{pending: pending, err: err}
	}
}

func (m chatModel) handleStep(msg stepMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
		m.mode = modeQuit
		return m, tea.Quit
	}

	switch msg.state {
	case runtime.StateAwaitingApproval:
		m.lastState = msg.state
		return m, m.loadPendingCmd()
	case runtime.StateCompleted, runtime.StateFailed, runtime.StateInterrupted:
		m.lastState = msg.state
		m.appendNewMessages()
		if msg.state == runtime.StateFailed {
			run, err := m.st.GetRun(m.ctx, m.runID)
			if err == nil && run.Error != nil {
				m.transcript = append(m.transcript, "run failed: "+*run.Error)
			}
		}
		if msg.state == runtime.StateInterrupted {
			// Unlike StateFailed, this isn't terminal — it stopped here
			// (rather than auto-retrying via the default case below) so an
			// interactive session doesn't silently hammer a rate-limited
			// API in the background. updateInput checks m.lastState and
			// resumes with a plain stepCmd on the next Enter, not
			// ContinueRun — ContinueRun rejects a non-terminal run outright
			// (see its own doc comment), and there's no new user turn to
			// add here anyway: the interrupted call was a retry of the
			// existing pending one.
			run, err := m.st.GetRun(m.ctx, m.runID)
			if err == nil && run.Error != nil {
				m.transcript = append(m.transcript, "run interrupted: "+*run.Error+" (press enter to retry)")
			}
		}
		m.mode = modeInput
		cmd := m.input.Focus()
		return m, cmd
	default:
		return m, m.stepCmd()
	}
}

func (m chatModel) handlePendingLoaded(msg pendingLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = msg.err
		m.mode = modeQuit
		return m, tea.Quit
	}
	m.pending = msg.pending
	m.pendingIdx = 0
	// Flush the tool_use (and any preceding text) that triggered this
	// pause now, not just at run completion, so the approval prompt
	// appears with context instead of out of nowhere.
	m.appendNewMessages()
	if len(m.pending) == 0 {
		m.mode = modeStepping
		return m, m.stepCmd()
	}
	m.mode = modeApproval
	return m, nil
}

func (m chatModel) updateApproval(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.pendingIdx >= len(m.pending) {
		m.mode = modeStepping
		return m, m.stepCmd()
	}
	call := m.pending[m.pendingIdx]

	switch msg.String() {
	case "y":
		return m.decide(call.ID, "approved", "")
	case "n":
		return m.decide(call.ID, "denied", "denied by user")
	case "a":
		for _, c := range m.pending[m.pendingIdx:] {
			if _, err := m.eng.RecordApproval(m.ctx, m.runID, c.ID, "approved", "user", ""); err != nil {
				m.err = err
				m.mode = modeQuit
				return m, tea.Quit
			}
		}
		m.transcript = append(m.transcript, fmt.Sprintf("approved all %d remaining call(s)", len(m.pending)-m.pendingIdx))
		m.mode = modeStepping
		return m, m.stepCmd()
	case "e":
		m.editInput.SetValue(call.ArgsJSON)
		m.editInput.CursorEnd()
		cmd := m.editInput.Focus()
		m.mode = modeEditInput
		return m, cmd
	}
	return m, nil
}

func (m chatModel) decide(callID, decision, reason string) (tea.Model, tea.Cmd) {
	if _, err := m.eng.RecordApproval(m.ctx, m.runID, callID, decision, "user", reason); err != nil {
		m.err = err
		m.mode = modeQuit
		return m, tea.Quit
	}
	m.transcript = append(m.transcript, fmt.Sprintf("%s: %s", decision, m.pending[m.pendingIdx].ToolName))
	m.pendingIdx++
	if m.pendingIdx >= len(m.pending) {
		m.mode = modeStepping
		return m, m.stepCmd()
	}
	return m, nil
}

func (m chatModel) updateEditInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		call := m.pending[m.pendingIdx]
		edited := m.editInput.Value()
		if !json.Valid([]byte(edited)) {
			m.transcript = append(m.transcript, "edit rejected: not valid JSON")
			return m, nil
		}
		if err := m.eng.EditPendingCallArgs(m.ctx, call.ID, json.RawMessage(edited)); err != nil {
			m.transcript = append(m.transcript, "edit failed: "+err.Error())
			return m, nil
		}
		m.transcript = append(m.transcript, fmt.Sprintf("edited args for %s: %s", call.ToolName, edited))
		m.mode = modeApproval
		return m.decide(call.ID, "approved", "")
	case tea.KeyEsc:
		m.mode = modeApproval
		return m, nil
	}

	var cmd tea.Cmd
	m.editInput, cmd = m.editInput.Update(msg)
	return m, cmd
}

// appendNewMessages renders any messages persisted since the last flush
// (tool calls and results included) so the transcript reflects the whole
// turn, not just the final assistant reply.
func (m *chatModel) appendNewMessages() {
	msgs, err := m.st.ListMessages(m.ctx, m.runID)
	if err != nil {
		m.transcript = append(m.transcript, "error loading messages: "+err.Error())
		return
	}
	for _, msg := range msgs[m.shownMsgCount:] {
		for _, b := range msg.Content {
			switch b.Type {
			case message.BlockText:
				if msg.Role == message.RoleAssistant {
					m.transcript = append(m.transcript, "agent: "+b.Text)
				}
			case message.BlockToolUse:
				m.transcript = append(m.transcript, fmt.Sprintf("agent: calling %s(%s)", b.Name, string(b.Input)))
			case message.BlockToolResult:
				label := "tool result"
				if b.IsError {
					label = "tool error"
				}
				m.transcript = append(m.transcript, fmt.Sprintf("%s: %s", label, b.Content))
			}
		}
	}
	m.shownMsgCount = len(msgs)
}

func (m chatModel) View() string {
	var b strings.Builder
	for _, line := range m.transcript {
		b.WriteString(line)
		b.WriteString("\n")
	}

	switch m.mode {
	case modeApproval:
		if m.pendingIdx < len(m.pending) {
			call := m.pending[m.pendingIdx]
			fmt.Fprintf(&b, "\napproval needed (%d/%d): %s\n  args: %s\n", m.pendingIdx+1, len(m.pending), call.ToolName, call.ArgsJSON)
			b.WriteString("[y]es  [n]o  [e]dit  [a]pprove all remaining\n")
		}
	case modeEditInput:
		b.WriteString("\nedit args, enter to confirm and approve, esc to cancel:\n")
		b.WriteString(m.editInput.View())
		b.WriteString("\n")
	case modeStepping:
		b.WriteString("\n...\n")
	case modeInput:
		b.WriteString("\n")
		b.WriteString(m.input.View())
		b.WriteString("\n")
	case modeQuit:
		if m.err != nil {
			b.WriteString("\nerror: " + m.err.Error() + "\n")
		}
	}
	return b.String()
}
