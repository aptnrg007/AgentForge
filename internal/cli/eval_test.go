package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentforge/internal/eval"
)

func writeEvalFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

const evalTestAgentYAML = `name: test-agent
model:
  provider: ollama
  name: test-model
instructions: "test agent"
limits:
  max_turns: 5
`

func TestSuiteFilesResolvesASingleFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "suite.yaml")
	writeEvalFile(t, path, "agent: agent.yaml\ncases: []\n")

	got, err := suiteFiles(path)
	if err != nil {
		t.Fatalf("suiteFiles: %v", err)
	}
	if len(got) != 1 || got[0] != path {
		t.Fatalf("suiteFiles = %v, want [%s]", got, path)
	}
}

func TestSuiteFilesResolvesADirectorySorted(t *testing.T) {
	dir := t.TempDir()
	writeEvalFile(t, filepath.Join(dir, "b.yaml"), "agent: agent.yaml\ncases: []\n")
	writeEvalFile(t, filepath.Join(dir, "a.yml"), "agent: agent.yaml\ncases: []\n")
	writeEvalFile(t, filepath.Join(dir, "notes.txt"), "ignore me")

	got, err := suiteFiles(dir)
	if err != nil {
		t.Fatalf("suiteFiles: %v", err)
	}
	want := []string{filepath.Join(dir, "a.yml"), filepath.Join(dir, "b.yaml")}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("suiteFiles = %v, want %v", got, want)
	}
}

func TestSuiteFilesRejectsEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := suiteFiles(dir); err == nil {
		t.Fatal("suiteFiles: expected an error for a directory with no suite files")
	}
}

func TestRunEvalPassesAMatchingCase(t *testing.T) {
	dir := t.TempDir()
	writeEvalFile(t, filepath.Join(dir, "agent.yaml"), evalTestAgentYAML)
	writeEvalFile(t, filepath.Join(dir, "fixture.json"), `{"turns":[{"response":{"Content":[{"type":"text","text":"hello there"}],"StopReason":"end_turn"}}]}`)
	writeEvalFile(t, filepath.Join(dir, "suite.yaml"), `agent: agent.yaml
cases:
  - name: greets
    input: hi
    fixture_file: fixture.json
    expect:
      final_state: completed
      output_contains: ["hello"]
`)

	err := runEval(context.Background(), filepath.Join(dir, "suite.yaml"), eval.RunOptions{Mode: eval.ModeReplay, Root: dir})
	if err != nil {
		t.Fatalf("runEval: %v", err)
	}
}

func TestRunEvalReturnsErrorWhenACaseFails(t *testing.T) {
	dir := t.TempDir()
	writeEvalFile(t, filepath.Join(dir, "agent.yaml"), evalTestAgentYAML)
	writeEvalFile(t, filepath.Join(dir, "fixture.json"), `{"turns":[{"response":{"Content":[{"type":"text","text":"hello there"}],"StopReason":"end_turn"}}]}`)
	writeEvalFile(t, filepath.Join(dir, "suite.yaml"), `agent: agent.yaml
cases:
  - name: greets
    input: hi
    fixture_file: fixture.json
    expect:
      output_contains: ["goodbye"]
`)

	err := runEval(context.Background(), filepath.Join(dir, "suite.yaml"), eval.RunOptions{Mode: eval.ModeReplay, Root: dir})
	if err == nil || !strings.Contains(err.Error(), "one or more cases failed") {
		t.Fatalf("runEval: got %v, want a 'cases failed' error", err)
	}
}
