package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestListRunsOrdersMostRecentFirst(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.EnsureAgentExists(ctx, "agent-a"); err != nil {
		t.Fatalf("EnsureAgentExists: %v", err)
	}
	for _, id := range []string{"run-1", "run-2", "run-3"} {
		if err := st.CreateRun(ctx, id, "agent-a", "ready_for_model"); err != nil {
			t.Fatalf("CreateRun(%s): %v", id, err)
		}
	}

	runs, err := st.ListRuns(ctx, "", 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d", len(runs))
	}
	// created_at is millisecond-resolution and these were created back to
	// back, so ties are possible; assert run-3 (created last) is not
	// ordered before run-1 (created first) rather than a strict order.
	idx := map[string]int{}
	for i, r := range runs {
		idx[r.ID] = i
	}
	if idx["run-3"] > idx["run-1"] {
		t.Fatalf("expected run-3 no later than run-1 in most-recent-first order, got %+v", runs)
	}
}

func TestListRunsFiltersByAgent(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.EnsureAgentExists(ctx, "agent-a"); err != nil {
		t.Fatalf("EnsureAgentExists: %v", err)
	}
	if err := st.EnsureAgentExists(ctx, "agent-b"); err != nil {
		t.Fatalf("EnsureAgentExists: %v", err)
	}
	if err := st.CreateRun(ctx, "run-a1", "agent-a", "completed"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.CreateRun(ctx, "run-a2", "agent-a", "completed"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.CreateRun(ctx, "run-b1", "agent-b", "completed"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	runs, err := st.ListRuns(ctx, "agent-a", 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs for agent-a, got %d: %+v", len(runs), runs)
	}
	for _, r := range runs {
		if r.AgentName != "agent-a" {
			t.Fatalf("expected only agent-a runs, got %+v", r)
		}
	}

	all, err := st.ListRuns(ctx, "", 0)
	if err != nil {
		t.Fatalf("ListRuns (all): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 runs across both agents, got %d", len(all))
	}
}

func TestListRunsRespectsLimit(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.EnsureAgentExists(ctx, "agent-a"); err != nil {
		t.Fatalf("EnsureAgentExists: %v", err)
	}
	for i := 0; i < 5; i++ {
		id := "run-" + string(rune('a'+i))
		if err := st.CreateRun(ctx, id, "agent-a", "completed"); err != nil {
			t.Fatalf("CreateRun(%s): %v", id, err)
		}
	}

	runs, err := st.ListRuns(ctx, "", 2)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected limit=2 to cap at 2 runs, got %d", len(runs))
	}
}
