// Package cli implements the agentforge command-line interface with cobra:
// serve, run, chat, agents (list/get/delete), runs (get/approve/deny/
// resume/cancel), eval. Every subcommand that touches agent or run state
// accepts --server to talk to a running daemon instead of opening the
// local SQLite store directly — eval is the one exception, since a suite
// builds and drives its own throwaway engine per case rather than
// operating on a run a daemon already owns.
package cli

import (
	"context"
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
	root.AddCommand(newEvalCmd())
	return root
}

// Execute runs the CLI and reports whether it succeeded. Errors are
// printed here (not left to cobra's default handling) so every failure
// path in the tool uses the same "error: ..." format. ctx is
// signal-derived (main.go installs SIGINT/SIGTERM) and reaches every
// subcommand via cmd.Context() — cobra's ExecuteContext propagates it to
// whichever command actually runs — so Ctrl-C during e.g. `run` cancels
// that command's ctx, not just `serve`'s.
func Execute(ctx context.Context) error {
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return err
	}
	return nil
}
