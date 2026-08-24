package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"agentforge/internal/message"
	"agentforge/internal/store"
)

// printMessages writes msgs to w. Takes an io.Writer (rather than always
// stdout) so --output/--output-format can redirect a text-mode trace to a
// file or keep it off stdout in JSON mode; every pre-existing caller that
// still wants stdout passes os.Stdout explicitly.
func printMessages(w io.Writer, msgs []message.Message) {
	for _, m := range msgs {
		for _, b := range m.Content {
			switch b.Type {
			case message.BlockText:
				fmt.Fprintf(w, "%s: %s\n", m.Role, b.Text)
			case message.BlockToolUse:
				fmt.Fprintf(w, "%s: tool_use %s(%s)\n", m.Role, b.Name, string(b.Input))
			case message.BlockToolResult:
				fmt.Fprintf(w, "%s: tool_result[%s] %s\n", m.Role, b.ToolUseID, b.Content)
			}
		}
	}
}

// toolCallRow is the print layer's view of a tool call, common to both the
// local store.ToolCall and the remote remoteToolCall shapes so
// printToolCalls doesn't need to care which one it's rendering.
type toolCallRow struct {
	ID         string
	ToolName   string
	Approval   string
	DecidedBy  *string
	Reason     *string
	Result     *string
	IsError    bool
	DurationMS int64
}

func printToolCalls(calls []toolCallRow) {
	for _, tc := range calls {
		result := "(no result yet)"
		if tc.Result != nil {
			result = *tc.Result
			if tc.IsError {
				result = "ERROR: " + result
			}
		}
		by := ""
		if tc.DecidedBy != nil {
			by = " decided_by=" + *tc.DecidedBy
			if tc.Reason != nil && *tc.Reason != "" {
				by += fmt.Sprintf(" (%s)", *tc.Reason)
			}
		}
		duration := ""
		if tc.DurationMS > 0 {
			duration = fmt.Sprintf(" duration=%dms", tc.DurationMS)
		}
		fmt.Printf("tool_call %s %s approval=%s%s%s -> %s\n", tc.ID, tc.ToolName, tc.Approval, by, duration, result)
	}
}

// messageRow is the print layer's view of a message for `runs get`,
// common to both the local store.MessageDetail and the remote
// remoteMessage shapes — a message plus the token/latency cost of the
// model call that produced it (0 for every role but assistant).
type messageRow struct {
	Role         message.Role
	Content      []message.ContentBlock
	InputTokens  int
	OutputTokens int
	LatencyMS    int64
}

// printMessagesDetailed is printMessages plus a trailing usage annotation
// on any message that has one (in practice, only an assistant message —
// see Store.AppendMessageWithUsage) — used by `runs get`, which is the
// one place per-message cost is worth showing; every other caller of
// printMessages (a `run`/`chat` outcome) doesn't carry this data at all.
func printMessagesDetailed(w io.Writer, msgs []messageRow) {
	for _, m := range msgs {
		usage := ""
		if m.InputTokens > 0 || m.OutputTokens > 0 || m.LatencyMS > 0 {
			usage = fmt.Sprintf(" [in=%d out=%d latency=%dms]", m.InputTokens, m.OutputTokens, m.LatencyMS)
		}
		for _, b := range m.Content {
			switch b.Type {
			case message.BlockText:
				fmt.Fprintf(w, "%s: %s%s\n", m.Role, b.Text, usage)
			case message.BlockToolUse:
				fmt.Fprintf(w, "%s: tool_use %s(%s)%s\n", m.Role, b.Name, string(b.Input), usage)
			case message.BlockToolResult:
				fmt.Fprintf(w, "%s: tool_result[%s] %s\n", m.Role, b.ToolUseID, b.Content)
			}
			usage = "" // only annotate once per message, not once per block
		}
	}
}

func printRunTrace(id, state string, turnCount int, errStr *string, msgs []messageRow, calls []toolCallRow) {
	fmt.Printf("run %s state=%s turns=%d\n", id, state, turnCount)
	if errStr != nil {
		fmt.Println("error:", *errStr)
	}
	printToolCalls(calls)
	printMessagesDetailed(os.Stdout, msgs)
}

// runRow is the print layer's view of a run for `runs list`, common to
// both the local store.Run and remote remoteRunSummary shapes.
type runRow struct {
	ID        string
	AgentName string
	State     string
	TurnCount int
	CreatedAt int64
}

func printStats(s *store.Stats, agentFilter string) {
	scope := "all agents"
	if agentFilter != "" {
		scope = "agent=" + agentFilter
	}
	fmt.Printf("runs (%s): %d total, %d completed, %d failed, %d cancelled, %d in flight\n",
		scope, s.TotalRuns, s.CompletedRuns, s.FailedRuns, s.CancelledRuns, s.OtherRuns)
	fmt.Printf("success rate: %.1f%%\n", s.SuccessRate()*100)
	fmt.Printf("avg turns/run: %.1f\n", s.AvgTurns)
	fmt.Printf("tool calls: %d total (%.1f/run), %d failed, failure rate %.1f%%\n",
		s.TotalToolCalls, s.AvgToolCalls, s.FailedToolCalls, s.ToolFailureRate()*100)
	fmt.Printf("tokens: %d in, %d out, %d total\n", s.InputTokens, s.OutputTokens, s.InputTokens+s.OutputTokens)
}

func printRunsList(rows []runRow) {
	if len(rows) == 0 {
		fmt.Println("no runs")
		return
	}
	for _, r := range rows {
		fmt.Printf("%s  %-17s  agent=%-20s turns=%-3d  %s\n",
			r.ID, r.State, r.AgentName, r.TurnCount, time.UnixMilli(r.CreatedAt).Format(time.RFC3339))
	}
}
