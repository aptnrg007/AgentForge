package cli

import (
	"fmt"

	"agentforge/internal/message"
)

func printMessages(msgs []message.Message) {
	for _, m := range msgs {
		for _, b := range m.Content {
			switch b.Type {
			case message.BlockText:
				fmt.Printf("%s: %s\n", m.Role, b.Text)
			case message.BlockToolUse:
				fmt.Printf("%s: tool_use %s(%s)\n", m.Role, b.Name, string(b.Input))
			case message.BlockToolResult:
				fmt.Printf("%s: tool_result[%s] %s\n", m.Role, b.ToolUseID, b.Content)
			}
		}
	}
}

func printRunTrace(id, state string, turnCount int, errStr *string, msgs []message.Message) {
	fmt.Printf("run %s state=%s turns=%d\n", id, state, turnCount)
	if errStr != nil {
		fmt.Println("error:", *errStr)
	}
	printMessages(msgs)
}
