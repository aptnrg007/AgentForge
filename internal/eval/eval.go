// Package eval implements the eval harness: a suite of scripted
// conversations run against a real agent config, asserting on the same
// data every run already persists (runs.state, runs.turn_count,
// runs.repair_count, tool_calls.tool_name) rather than inventing new
// runtime machinery. See docs/DESIGN.md section 12.
//
// A Suite (one examples/evals/*.yaml file) names an agent config and a
// list of Cases. Each Case sends one message and asserts on how the run
// ended. Load reads and validates a Suite the same way config.Load does
// for an agent config: sigs.k8s.io/yaml + UnmarshalStrict, so a typo'd
// key is a load error, not a silently-ignored no-op.
package eval

import (
	"fmt"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// Suite is one *.yaml file under examples/evals/.
type Suite struct {
	// Agent is a path to the agent config this suite exercises, resolved
	// relative to the suite file's own directory (like config.Config's
	// output.schema is resolved relative to the agent config's directory).
	Agent string `json:"agent"`
	Cases []Case `json:"cases"`

	// SourceDir is the directory Load read this suite from, used to
	// resolve Agent and every Case's FixtureFile. json:"-" keeps it
	// invisible to UnmarshalStrict's unknown-key check.
	SourceDir string `json:"-"`
	// Path is the suite file itself, kept for reporting.
	Path string `json:"-"`
}

// Case is one scripted conversation: send Input, then check the run
// against Expect once it reaches a stop point.
type Case struct {
	Name   string `json:"name"`
	Input  string `json:"input"`
	Expect Expect `json:"expect"`
	// FixtureFile overrides the default fixture path
	// (testdata/fixtures/<suite-name>/<case-name>.json) for --replay mode
	// and --live --record's output. Rarely needed — two cases with the
	// same Name in the same suite is the only reason to set it.
	FixtureFile string `json:"fixture_file,omitempty"`
}

// Expect is a Case's assertions. Every field is optional; an empty Expect
// only checks that the run reached some terminal or awaiting_approval
// state without error.
type Expect struct {
	// FinalState is one of completed | failed | cancelled |
	// awaiting_approval | interrupted. Empty means "don't care".
	FinalState string `json:"final_state,omitempty"`
	// ToolCalled lists tool names (namespaced, e.g. "geo.search") that
	// must appear at least once in the run's tool_calls.
	ToolCalled []string `json:"tool_called,omitempty"`
	// ToolNotCalled lists tool names that must NOT appear.
	ToolNotCalled []string `json:"tool_not_called,omitempty"`
	// OutputContains lists substrings the final assistant message's text
	// must all contain.
	OutputContains []string `json:"output_contains,omitempty"`
	// MaxTurns caps runs.turn_count; 0 means unchecked.
	MaxTurns int `json:"max_turns,omitempty"`
	// NoRepairs requires runs.repair_count == 0 (no tool-call arg repair
	// or schema-validation retry happened).
	NoRepairs bool `json:"no_repairs,omitempty"`
	// OutputMatchesSchema is a path to a JSON Schema file, resolved
	// relative to the suite file's directory, that the final assistant
	// message's extracted JSON must validate against.
	OutputMatchesSchema string `json:"output_matches_schema,omitempty"`
}

// Load reads and validates a Suite from path.
func Load(path string) (*Suite, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read %s: %w", path, err)
	}
	var s Suite
	if err := yaml.UnmarshalStrict(raw, &s); err != nil {
		return nil, fmt.Errorf("eval: %s: %w", path, err)
	}
	if s.Agent == "" {
		return nil, fmt.Errorf("eval: %s: agent is required", path)
	}
	if len(s.Cases) == 0 {
		return nil, fmt.Errorf("eval: %s: at least one case is required", path)
	}
	seen := make(map[string]bool, len(s.Cases))
	for _, c := range s.Cases {
		if c.Name == "" {
			return nil, fmt.Errorf("eval: %s: every case needs a name", path)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("eval: %s: duplicate case name %q", path, c.Name)
		}
		seen[c.Name] = true
		if c.Input == "" {
			return nil, fmt.Errorf("eval: %s: case %q: input is required", path, c.Name)
		}
	}
	s.SourceDir = filepath.Dir(path)
	s.Path = path
	return &s, nil
}

// AgentPath resolves Suite.Agent against SourceDir.
func (s *Suite) AgentPath() string {
	if filepath.IsAbs(s.Agent) {
		return s.Agent
	}
	return filepath.Join(s.SourceDir, s.Agent)
}

// FixturePath resolves c's fixture file: FixtureFile if set, otherwise
// the default testdata/fixtures/<suite base name>/<case name>.json,
// relative to root (typically the repo root the eval command was
// invoked from).
func (s *Suite) FixturePath(c Case, root string) string {
	if c.FixtureFile != "" {
		if filepath.IsAbs(c.FixtureFile) {
			return c.FixtureFile
		}
		return filepath.Join(s.SourceDir, c.FixtureFile)
	}
	suiteName := filepath.Base(s.Path)
	suiteName = suiteName[:len(suiteName)-len(filepath.Ext(suiteName))]
	return filepath.Join(root, "testdata", "fixtures", suiteName, sanitizeFilename(c.Name)+".json")
}

func sanitizeFilename(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		case r == ' ':
			out = append(out, '-')
		}
	}
	return string(out)
}
