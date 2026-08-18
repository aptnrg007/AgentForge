// Package store persists run state to SQLite so a run can be resumed after
// process restart. No ORM: plain database/sql with hand-written queries.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"agentforge/internal/message"
)

//go:embed schema.sql
var schemaSQL string

const schemaVersion = 1

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// modernc.org/sqlite serializes internally; a single connection avoids
	// SQLITE_BUSY errors on concurrent writers from the same process.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: enable foreign keys: %w", err)
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: apply schema: %w", err)
	}

	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_version").Scan(&count); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: read schema_version: %w", err)
	}
	if count == 0 {
		if _, err := db.Exec("INSERT INTO schema_version (version) VALUES (?)", schemaVersion); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: seed schema_version: %w", err)
		}
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func now() int64 { return time.Now().UnixMilli() }

// --- agents ---

func (s *Store) UpsertAgent(ctx context.Context, name, yaml string) error {
	t := now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agents (name, yaml, created_at, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET yaml = excluded.yaml, updated_at = excluded.updated_at
	`, name, yaml, t, t)
	if err != nil {
		return fmt.Errorf("store: upsert agent %s: %w", name, err)
	}
	return nil
}

// --- runs ---

type Run struct {
	ID          string
	AgentName   string
	State       string
	TurnCount   int
	RepairCount int
	Error       *string
	CreatedAt   int64
	UpdatedAt   int64
}

func (s *Store) CreateRun(ctx context.Context, id, agentName, state string) error {
	t := now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (id, agent_name, state, turn_count, repair_count, created_at, updated_at)
		VALUES (?, ?, ?, 0, 0, ?, ?)
	`, id, agentName, state, t, t)
	if err != nil {
		return fmt.Errorf("store: create run %s: %w", id, err)
	}
	return nil
}

func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	var r Run
	err := s.db.QueryRowContext(ctx, `
		SELECT id, agent_name, state, turn_count, repair_count, error, created_at, updated_at
		FROM runs WHERE id = ?
	`, id).Scan(&r.ID, &r.AgentName, &r.State, &r.TurnCount, &r.RepairCount, &r.Error, &r.CreatedAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("store: run %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get run %s: %w", id, err)
	}
	return &r, nil
}

func (s *Store) UpdateRun(ctx context.Context, id, state string, turnCount, repairCount int, errStr *string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE runs SET state = ?, turn_count = ?, repair_count = ?, error = ?, updated_at = ?
		WHERE id = ?
	`, state, turnCount, repairCount, errStr, now(), id)
	if err != nil {
		return fmt.Errorf("store: update run %s: %w", id, err)
	}
	return nil
}

// --- messages ---

func (s *Store) AppendMessage(ctx context.Context, runID string, msg message.Message) (int, error) {
	var maxSeq sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(seq) FROM messages WHERE run_id = ?`, runID).Scan(&maxSeq); err != nil {
		return 0, fmt.Errorf("store: next seq for run %s: %w", runID, err)
	}
	seq := int(maxSeq.Int64) + 1

	contentJSON, err := json.Marshal(msg.Content)
	if err != nil {
		return 0, fmt.Errorf("store: marshal message content: %w", err)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO messages (run_id, seq, role, content_json, created_at) VALUES (?, ?, ?, ?, ?)
	`, runID, seq, string(msg.Role), string(contentJSON), now())
	if err != nil {
		return 0, fmt.Errorf("store: append message to run %s: %w", runID, err)
	}
	return seq, nil
}

func (s *Store) ListMessages(ctx context.Context, runID string) ([]message.Message, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content_json FROM messages WHERE run_id = ? ORDER BY seq ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list messages for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []message.Message
	for rows.Next() {
		var role, contentJSON string
		if err := rows.Scan(&role, &contentJSON); err != nil {
			return nil, fmt.Errorf("store: scan message: %w", err)
		}
		var content []message.ContentBlock
		if err := json.Unmarshal([]byte(contentJSON), &content); err != nil {
			return nil, fmt.Errorf("store: unmarshal message content: %w", err)
		}
		out = append(out, message.Message{Role: message.Role(role), Content: content})
	}
	return out, rows.Err()
}

// --- tool calls ---

type ToolCall struct {
	ID         string
	RunID      string
	ToolName   string
	ArgsJSON   string
	Approval   string
	Result     *string
	IsError    bool
	CreatedAt  int64
	ExecutedAt *int64
}

func (s *Store) InsertToolCall(ctx context.Context, tc ToolCall) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tool_calls (id, run_id, tool_name, args_json, approval, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, tc.ID, tc.RunID, tc.ToolName, tc.ArgsJSON, tc.Approval, now())
	if err != nil {
		return fmt.Errorf("store: insert tool call %s: %w", tc.ID, err)
	}
	return nil
}

// ListPendingToolCalls returns tool calls that are approved/auto but not yet executed.
func (s *Store) ListPendingToolCalls(ctx context.Context, runID string) ([]ToolCall, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, tool_name, args_json, approval
		FROM tool_calls
		WHERE run_id = ? AND approval IN ('auto', 'approved') AND result_json IS NULL
		ORDER BY created_at ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list pending tool calls for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []ToolCall
	for rows.Next() {
		var tc ToolCall
		if err := rows.Scan(&tc.ID, &tc.RunID, &tc.ToolName, &tc.ArgsJSON, &tc.Approval); err != nil {
			return nil, fmt.Errorf("store: scan tool call: %w", err)
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

func (s *Store) UpdateToolCallResult(ctx context.Context, id, result string, isError bool) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tool_calls SET result_json = ?, is_error = ?, executed_at = ? WHERE id = ?
	`, result, isError, now(), id)
	if err != nil {
		return fmt.Errorf("store: update tool call result %s: %w", id, err)
	}
	return nil
}

func (s *Store) ListToolCalls(ctx context.Context, runID string) ([]ToolCall, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, tool_name, args_json, approval, result_json, is_error, created_at, executed_at
		FROM tool_calls WHERE run_id = ? ORDER BY created_at ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list tool calls for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []ToolCall
	for rows.Next() {
		var tc ToolCall
		var isErrorInt int
		if err := rows.Scan(&tc.ID, &tc.RunID, &tc.ToolName, &tc.ArgsJSON, &tc.Approval, &tc.Result, &isErrorInt, &tc.CreatedAt, &tc.ExecutedAt); err != nil {
			return nil, fmt.Errorf("store: scan tool call: %w", err)
		}
		tc.IsError = isErrorInt != 0
		out = append(out, tc)
	}
	return out, rows.Err()
}
