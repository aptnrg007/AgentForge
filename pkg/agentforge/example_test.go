package agentforge_test

import (
	"context"
	"fmt"
	"log"

	"agentforge/pkg/agentforge"
)

// Example mirrors the package doc's usage sample verbatim — compiled
// (proving the public API shape actually matches the docs) but not
// executed, since it has no "Output:" comment and needs a real model to
// run against.
func Example() {
	ag, err := agentforge.Load("agent.yaml")
	if err != nil {
		log.Fatal(err)
	}
	defer ag.Close()

	run, err := ag.Run(context.Background(), "Find the latest issues")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(run.Output)
}
