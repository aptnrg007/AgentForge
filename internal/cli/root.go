// Package cli implements the agentforge command-line interface with cobra:
// serve, run, agents (list/get/delete), runs (get). Every subcommand that
// touches agent or run state accepts --server to talk to a running daemon
// instead of opening the local SQLite store directly.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "agentforge",
		Short:         "YAML in, agent out. MCP tools, runs on your own hardware.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServeCmd())
	root.AddCommand(newRunCmd())
	root.AddCommand(newChatCmd())
	root.AddCommand(newAgentsCmd())
	root.AddCommand(newRunsCmd())
	return root
}

// Execute runs the CLI and reports whether it succeeded. Errors are
// printed here (not left to cobra's default handling) so every failure
// path in the tool uses the same "error: ..." format.
func Execute() error {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return err
	}
	return nil
}
