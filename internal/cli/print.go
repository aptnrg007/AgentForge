package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"agentforge/internal/message"
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
	ID        string
	ToolName  string
	Approval  string
	DecidedBy *string
	Reason    *string
	Result    *string
	IsError   bool
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
		fmt.Printf("tool_call %s %s approval=%s%s -> %s\n", tc.ID, tc.ToolName, tc.Approval, by, result)
	}
}

func printRunTrace(id, state string, turnCount int, errStr *string, msgs []message.Message, calls []toolCallRow) {
	fmt.Printf("run %s state=%s turns=%d\n", id, state, turnCount)
	if errStr != nil {
		fmt.Println("error:", *errStr)
	}
	printToolCalls(calls)
	printMessages(os.Stdout, msgs)
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
