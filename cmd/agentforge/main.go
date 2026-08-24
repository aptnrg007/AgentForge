package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"agentforge/internal/cli"
)

func main() {
	// One signal-derived context for the whole CLI, not just `serve`:
	// every subcommand's ctx.Done() now fires on Ctrl-C, so a run that's
	// mid-flight (blocked on a provider HTTP call, an MCP tool call) gets
	// a chance to cancel cleanly — persist StateCancelled, tear down MCP
	// subprocesses via registry.Close()'s deferred call — instead of the
	// process just dying and leaving both stranded. See
	// docs/DESIGN.md ground rule 1 and internal/runtime.Engine.Cancel.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := cli.Execute(ctx); err != nil {
		os.Exit(1)
	}
}
