package store

import (
	"database/sql"
	"fmt"
)

// schemaVersion is the current schema, i.e. what schema.sql's CREATE
// TABLE statements already produce for a brand-new database. A DB
// created by an older binary needs migrations, below, to reach the same
// shape — schema.sql's CREATE TABLE IF NOT EXISTS is a no-op against a
// table that already exists, so it can't add a column to one.
const schemaVersion = 2

// migration adds a schema_version's worth of ALTER TABLE statements,
// applied to a database that predates it. A migration's Stmts must bring
// an already-schemaSQL'd-once database from Version-1 to Version — schema.sql
// itself only ever describes the current version, never an older one.
type migration struct {
	version int
	stmts   []string
}

// migrations is applied in order; every entry's version must be unique
// and increasing, checked once in TestMigrationsAreOrdered rather than on
// every Open.
var migrations = []migration{
	{
		version: 2,
		stmts: []string{
			`ALTER TABLE messages ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE messages ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE messages ADD COLUMN latency_ms INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE tool_calls ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0`,
		},
	},
}

// migrateSchema brings db up to schemaVersion. schema.sql has already run
// by the time this is called, so a brand-new database (schema_version
// empty) is already in its final shape — schema_version just needs
// seeding, not migrating. An existing database instead steps through
// every migrations entry newer than its recorded version, in order.
func migrateSchema(db *sql.DB) error {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&count); err != nil {
		return fmt.Errorf("store: read schema_version: %w", err)
	}
	if count == 0 {
		if _, err := db.Exec("INSERT INTO schema_version (version) VALUES (?)", schemaVersion); err != nil {
			return fmt.Errorf("store: seed schema_version: %w", err)
		}
		return nil
	}

	var current int
	if err := db.QueryRow("SELECT version FROM schema_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read schema_version: %w", err)
	}
	if current > schemaVersion {
		return fmt.Errorf("store: database schema version %d is newer than this binary supports (%d) — upgrade agentforge", current, schemaVersion)
	}
	if current == schemaVersion {
		return nil
	}

	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		for _, stmt := range m.stmts {
			if _, err := db.Exec(stmt); err != nil {
				return fmt.Errorf("store: migrate to schema version %d: %w", m.version, err)
			}
		}
		current = m.version
	}
	if current != schemaVersion {
		return fmt.Errorf("store: schema version %d after applying every migration, want %d — a migration is missing from internal/store/migrate.go", current, schemaVersion)
	}
	if _, err := db.Exec("UPDATE schema_version SET version = ?", schemaVersion); err != nil {
		return fmt.Errorf("store: update schema_version: %w", err)
	}
	return nil
}
