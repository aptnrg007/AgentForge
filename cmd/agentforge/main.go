package main

import (
	"fmt"
	"os"

	"agentforge/internal/cli"
)

func main() {
	args := os.Args[1:]

	run := cli.Run
	if len(args) > 0 && args[0] == "serve" {
		run, args = cli.Serve, args[1:]
	}

	if err := run(args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
