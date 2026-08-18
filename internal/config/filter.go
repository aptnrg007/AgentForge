package config

import (
	"path"

	"agentforge/internal/runtime"
)

// FilterTools keeps only the tools whose namespaced name (e.g.
// "github.search") matches at least one of the given glob patterns. An
// empty pattern list means no restriction: every tool passes through, so
// omitting `tools:` doesn't silently disable everything.
func FilterTools(tools []runtime.Tool, patterns []string) []runtime.Tool {
	if len(patterns) == 0 {
		return tools
	}
	out := make([]runtime.Tool, 0, len(tools))
	for _, t := range tools {
		for _, p := range patterns {
			if ok, _ := path.Match(p, t.Name); ok {
				out = append(out, t)
				break
			}
		}
	}
	return out
}
