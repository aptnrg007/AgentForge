package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"agentforge/internal/message"
	"agentforge/internal/provider"
)

// slowFakeProvider wraps fakeProvider's single-response reply with an
// artificial delay, so the latency stepModel records is reliably
// non-zero instead of racing a sub-millisecond in-process call.
type slowFakeProvider struct {
	delay time.Duration
	resp  *provider.Response
}

func (p slowFakeProvider) Name() string { return "slow-fake" }

func (p slowFakeProvider) Complete(ctx context.Context, r provider.Request) (*provider.Response, error) {
	time.Sleep(p.delay)
	return p.resp, nil
}

func (p slowFakeProvider) Stream(ctx context.Context, r provider.Request) (provider.Stream, error) {
	resp, err := p.Complete(ctx, r)
	if err != nil {
		return nil, err
	}
	return provider.NewResponseStream(resp), nil
}

func (p slowFakeProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }

// TestStepModelPersistsUsageAndLatencyOnTheAssistantMessage exercises
// AppendMessageWithUsage's wiring in stepModel end to end: a provider
// response's Usage must land on the assistant message row (not the user
// message that preceded it, and not silently dropped), and latency must
// reflect that the call actually took some measurable time.
func TestStepModelPersistsUsageAndLatencyOnTheAssistantMessage(t *testing.T) {
	st := openTestStore(t, filepath.Join(t.TempDir(), "test.db"))
	fp := slowFakeProvider{
		delay: 20 * time.Millisecond,
		resp: &provider.Response{
			Content:    []message.ContentBlock{{Type: message.BlockText, Text: "done"}},
			StopReason: "end_turn",
			Usage:      provider.Usage{InputTokens: 123, OutputTokens: 45},
		},
	}
	eng := NewEngine(st, fp, Config{AgentName: "a", Model: "m", MaxTurns: 10})

	ctx := context.Background()
	if err := eng.NewRun(ctx, "r", "go"); err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	state, err := eng.Step(ctx, "r")
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if state != StateCompleted {
		t.Fatalf("state = %s, want completed", state)
	}

	details, err := st.ListMessagesDetailed(ctx, "r")
	if err != nil {
		t.Fatalf("ListMessagesDetailed: %v", err)
	}
	if len(details) != 2 {
		t.Fatalf("got %d messages, want 2 (user + assistant)", len(details))
	}

	userMsg, assistantMsg := details[0], details[1]
	if userMsg.InputTokens != 0 || userMsg.OutputTokens != 0 || userMsg.LatencyMS != 0 {
		t.Fatalf("user message usage = %+v, want all zero", userMsg)
	}
	if assistantMsg.InputTokens != 123 || assistantMsg.OutputTokens != 45 {
		t.Fatalf("assistant message tokens = in=%d out=%d, want in=123 out=45", assistantMsg.InputTokens, assistantMsg.OutputTokens)
	}
	if assistantMsg.LatencyMS < 20 {
		t.Fatalf("assistant message latency = %dms, want >= 20ms (the provider's artificial delay)", assistantMsg.LatencyMS)
	}
}
