package agentforge

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"

	"agentforge/internal/agent"
	"agentforge/internal/message"
	"agentforge/internal/runtime"
)

// Run is one run's outcome as of the moment Run/Resume returned:
// completed, failed, cancelled, awaiting_approval (see Pending), or
// interrupted (see Resumable).
type Run struct {
	ID    string
	State string
	// Output is the final assistant message's text, set only when State
	// is "completed". For a structured-output agent (output.schema),
	// this is the raw text the model produced — still worth parsing as
	// JSON yourself if you need the typed value, since Run doesn't know
	// the schema's Go shape.
	Output string
	// Error explains a "failed" or "interrupted" State; empty otherwise.
	Error string
	// Pending lists the tool calls awaiting a decision when State is
	// "awaiting_approval" — resolve each with Approve or Deny, then call
	// Resume.
	Pending []PendingCall
	// Resumable is true only when State is "interrupted": the run's
	// retry budget ran out on what looked like a transient provider
	// error (a rate limit, an outage), so it's left non-terminal —
	// call Resume once the condition has had a chance to clear, no
	// Approve/Deny needed first. Always false for every other State,
	// including "awaiting_approval" (see Pending instead).
	Resumable bool
}

// PendingCall is one tool call waiting on a human (or programmatic)
// approval decision.
type PendingCall struct {
	CallID string
	Tool   string
	Args   json.RawMessage
}

// Run starts a new run with userMessage and drives it to its next stop
// point — completed, failed, cancelled, awaiting_approval, or interrupted.
func (a *Agent) Run(ctx context.Context, userMessage string, opts ...RunOption) (*Run, error) {
	var o runOptions
	for _, opt := range opts {
		opt(&o)
	}

	eng, err := agent.Build(ctx, a.st, a.registry, a.cfg, a.pf)
	if err != nil {
		return nil, err
	}
	if o.onEvent != nil {
		eng.OnEvent(o.onEvent)
	}

	runID := newRunID()
	if err := eng.NewRun(ctx, runID, userMessage); err != nil {
		return nil, err
	}

	state, err := eng.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	return a.buildRun(ctx, runID, state)
}

// Resume continues an existing run — most commonly one sitting at
// awaiting_approval whose pending calls have all been decided (see
// Approve/Deny) — until its next stop point. runID must belong to this
// Agent's own config; a run started against a different agent config (or
// through the CLI/HTTP API with a config this Agent wasn't Load-ed from)
// isn't guaranteed to resume correctly here — use `agentforge runs
// resume` or the HTTP API for that case, which reconstruct the engine
// from the run's own persisted config instead of assuming the caller's.
func (a *Agent) Resume(ctx context.Context, runID string, opts ...RunOption) (*Run, error) {
	var o runOptions
	for _, opt := range opts {
		opt(&o)
	}

	eng, err := agent.Build(ctx, a.st, a.registry, a.cfg, a.pf)
	if err != nil {
		return nil, err
	}
	if o.onEvent != nil {
		eng.OnEvent(o.onEvent)
	}

	state, err := eng.Run(ctx, runID)
	if err != nil {
		return nil, err
	}
	return a.buildRun(ctx, runID, state)
}

// Approve records an approval decision on a pending tool call. It does
// not itself continue the run — call Resume for that — so several
// pending calls can be decided before driving the run forward again.
func (a *Agent) Approve(ctx context.Context, runID, callID, reason string) error {
	return a.decide(ctx, runID, callID, "approved", reason)
}

// Deny records a denial decision on a pending tool call — the run isn't
// stopped by this either; the denial is fed back to the model as a
// tool-result error on the next Resume, and the model can try something
// else.
func (a *Agent) Deny(ctx context.Context, runID, callID, reason string) error {
	return a.decide(ctx, runID, callID, "denied", reason)
}

func (a *Agent) decide(ctx context.Context, runID, callID, decision, reason string) error {
	eng, err := agent.Build(ctx, a.st, a.registry, a.cfg, a.pf)
	if err != nil {
		return err
	}
	_, err = eng.RecordApproval(ctx, runID, callID, decision, "sdk", reason)
	return err
}

// Cancel stops a non-terminal run immediately, returning the state it
// ended up in ("cancelled") — or an error if the run was already
// terminal. Unlike Run/Resume/Approve/Deny, this doesn't build a real
// engine (Cancel only touches persisted run state), so it works even
// against a run whose agent config is currently broken.
func (a *Agent) Cancel(ctx context.Context, runID string) (string, error) {
	eng := runtime.NewEngine(a.st, nil, runtime.Config{})
	state, err := eng.Cancel(ctx, runID)
	return string(state), err
}

func (a *Agent) buildRun(ctx context.Context, runID string, state runtime.State) (*Run, error) {
	run, err := a.st.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := &Run{ID: runID, State: string(state)}
	if run.Error != nil {
		out.Error = *run.Error
	}

	switch state {
	case runtime.StateCompleted:
		msgs, err := a.st.ListMessages(ctx, runID)
		if err != nil {
			return nil, err
		}
		out.Output = finalAssistantText(msgs)
	case runtime.StateAwaitingApproval:
		pending, err := a.st.ListPendingApprovals(ctx, runID)
		if err != nil {
			return nil, err
		}
		out.Pending = make([]PendingCall, len(pending))
		for i, tc := range pending {
			out.Pending[i] = PendingCall{CallID: tc.ID, Tool: tc.ToolName, Args: json.RawMessage(tc.ArgsJSON)}
		}
	case runtime.StateInterrupted:
		out.Resumable = true
	}
	return out, nil
}

// finalAssistantText concatenates the last assistant message's text
// blocks — the run's final answer.
func finalAssistantText(msgs []message.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != message.RoleAssistant {
			continue
		}
		text := ""
		for _, block := range msgs[i].Content {
			if block.Type == message.BlockText {
				text += block.Text
			}
		}
		return text
	}
	return ""
}

func newRunID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("run_%x", b)
}
