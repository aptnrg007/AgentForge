package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"agentforge/internal/message"
)

// v1SchemaSQL is schema.sql as it existed before schema_version 2 added
// input_tokens/output_tokens/latency_ms (messages) and duration_ms
// (tool_calls) — kept here, not read from git history, specifically so
// this test keeps exercising the migration path even after a future
// schema_version 3 changes schema.sql again.
const v1SchemaSQL = `
CREATE TABLE IF NOT EXISTS schema_version (
  version INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS agents (
  name       TEXT PRIMARY KEY,
  yaml       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
  id           TEXT PRIMARY KEY,
  agent_name   TEXT NOT NULL REFERENCES agents(name),
  state        TEXT NOT NULL,
  turn_count   INTEGER NOT NULL DEFAULT 0,
  repair_count INTEGER NOT NULL DEFAULT 0,
  error        TEXT,
  created_at   INTEGER NOT NULL,
  updated_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
  run_id       TEXT NOT NULL REFERENCES runs(id),
  seq          INTEGER NOT NULL,
  role         TEXT NOT NULL,
  content_json TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS tool_calls (
  id          TEXT PRIMARY KEY,
  run_id      TEXT NOT NULL REFERENCES runs(id),
  tool_name   TEXT NOT NULL,
  args_json   TEXT NOT NULL,
  approval    TEXT NOT NULL,
  decided_by  TEXT,
  reason      TEXT,
  result_json TEXT,
  is_error    INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  decided_at  INTEGER,
  executed_at INTEGER
);

CREATE INDEX IF NOT EXISTS idx_runs_agent ON runs(agent_name, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_toolcalls_run ON tool_calls(run_id, created_at);
`

// newV1Database creates a database file at path already shaped like
// schema_version 1 (pre-migration) — a real pre-existing agentforge.db
// from before this migration existed, not something Open produced.
func newV1Database(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(v1SchemaSQL); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}
	if _, err := db.Exec("INSERT INTO schema_version (version) VALUES (1)"); err != nil {
		t.Fatalf("seed schema_version: %v", err)
	}
}

func TestOpenMigratesAPreExistingV1Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	newV1Database(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open a pre-migration database: %v", err)
	}
	defer st.Close()

	var version int
	if err := st.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version after Open: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("schema_version = %d after Open, want %d", version, schemaVersion)
	}

	// The migrated columns must actually be usable, not just present.
	ctx := context.Background()
	if err := st.UpsertAgent(ctx, "a", "name: a\n"); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	if err := st.CreateRun(ctx, "r1", "a", "ready_for_model"); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.AppendMessageWithUsage(ctx, "r1", message.Text(message.RoleAssistant, "hi"), 10, 20, 500); err != nil {
		t.Fatalf("AppendMessageWithUsage on a migrated database: %v", err)
	}
	details, err := st.ListMessagesDetailed(ctx, "r1")
	if err != nil {
		t.Fatalf("ListMessagesDetailed: %v", err)
	}
	if len(details) != 1 || details[0].InputTokens != 10 || details[0].OutputTokens != 20 || details[0].LatencyMS != 500 {
		t.Fatalf("ListMessagesDetailed = %+v, want one row with input=10 output=20 latency=500", details)
	}
}

func TestOpenRejectsAFutureSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if _, err := db.Exec(v1SchemaSQL); err != nil {
		t.Fatalf("apply v1 schema: %v", err)
	}
	if _, err := db.Exec("INSERT INTO schema_version (version) VALUES (?)", schemaVersion+1); err != nil {
		t.Fatalf("seed schema_version: %v", err)
	}
	db.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("Open: expected an error opening a database from a newer schema version")
	}
}

// TestOpenMigratesAPreExistingV2DatabaseWithNoDDL covers the empty
// migration entry that bumped schemaVersion to 3 for runtime.
// StateInterrupted: unlike every other migration here, it adds no
// columns (runs.state is a plain TEXT column with no CHECK constraint —
// a new state value needs no schema change at all), so its only job is
// updating schema_version itself. A v2 database — schema.sql's current
// CREATE TABLE shape already matches v2 exactly, since v3 adds no DDL —
// must open cleanly and land on schema_version 3.
func TestOpenMigratesAPreExistingV2DatabaseWithNoDDL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := db.Exec("INSERT INTO schema_version (version) VALUES (2)"); err != nil {
		t.Fatalf("seed schema_version: %v", err)
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open a v2 database: %v", err)
	}
	defer st.Close()

	var version int
	if err := st.db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatalf("read schema_version after Open: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("schema_version = %d after Open, want %d", version, schemaVersion)
	}

	// A run with the new state must round-trip normally — nothing about
	// v3 constrains runs.state's possible values.
	ctx := context.Background()
	if err := st.UpsertAgent(ctx, "a", "name: a\n"); err != nil {
		t.Fatalf("UpsertAgent: %v", err)
	}
	if err := st.CreateRun(ctx, "r1", "a", "interrupted"); err != nil {
		t.Fatalf("CreateRun with state=interrupted on a migrated database: %v", err)
	}
	run, err := st.GetRun(ctx, "r1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.State != "interrupted" {
		t.Fatalf("run.State = %q, want %q", run.State, "interrupted")
	}
}

func TestMigrationsAreOrderedAndReachSchemaVersion(t *testing.T) {
	last := 1 // schema_version before the first registered migration
	for _, m := range migrations {
		if m.version <= last {
			t.Fatalf("migrations must be strictly increasing: version %d follows %d", m.version, last)
		}
		last = m.version
	}
	if last != schemaVersion {
		t.Fatalf("last migration targets version %d, want schemaVersion %d", last, schemaVersion)
	}
}
