package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestLoadRequiresAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "suite.yaml")
	writeFile(t, path, `cases:
  - name: c1
    input: hi
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "agent is required") {
		t.Fatalf("Load: got %v, want an 'agent is required' error", err)
	}
}

func TestLoadRequiresAtLeastOneCase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "suite.yaml")
	writeFile(t, path, `agent: agent.yaml
cases: []
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "at least one case") {
		t.Fatalf("Load: got %v, want an 'at least one case' error", err)
	}
}

func TestLoadRejectsDuplicateCaseNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "suite.yaml")
	writeFile(t, path, `agent: agent.yaml
cases:
  - name: dup
    input: hi
  - name: dup
    input: there
`)
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "duplicate case name") {
		t.Fatalf("Load: got %v, want a 'duplicate case name' error", err)
	}
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "suite.yaml")
	writeFile(t, path, `agent: agent.yaml
bogus: true
cases:
  - name: c1
    input: hi
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load: expected an error for an unknown top-level key")
	}
}

func TestFixturePathDefaultsUnderTestdataFixtures(t *testing.T) {
	s := &Suite{Path: "/repo/examples/evals/weather.yaml", SourceDir: "/repo/examples/evals"}
	got := s.FixturePath(Case{Name: "resolves a city"}, "/repo")
	want := "/repo/testdata/fixtures/weather/resolves-a-city.json"
	if got != want {
		t.Fatalf("FixturePath = %q, want %q", got, want)
	}
}

func TestFixturePathHonorsOverride(t *testing.T) {
	s := &Suite{Path: "/repo/examples/evals/weather.yaml", SourceDir: "/repo/examples/evals"}
	got := s.FixturePath(Case{Name: "x", FixtureFile: "custom.json"}, "/repo")
	want := "/repo/examples/evals/custom.json"
	if got != want {
		t.Fatalf("FixturePath = %q, want %q", got, want)
	}
}
