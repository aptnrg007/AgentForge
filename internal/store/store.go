// Package store persists run state to SQLite so a run can be resumed after
// process restart. No ORM: plain database/sql with hand-written queries.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"agentforge/internal/message"
)

//go:embed schema.sql
var schemaSQL string

const schemaVersion = 1

// ErrNotFound is returned by lookups (GetAgent, GetRun) when no row
// matches, so callers can distinguish "not found" from other failures
// (e.g. to map it to an HTTP 404).
var ErrNotFound = errors.New("not found")

// ErrNotPending is returned by DecideToolCall when the call has already
// been decided (or never needed a decision), so callers can distinguish a
// stale/duplicate decision from an unknown call ID.
var ErrNotPending = errors.New("tool call is not pending approval")

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
	// WAL + a busy_timeout let a CLI command (agents/runs/run) and a
	// running daemon safely open the same database file concurrently —
	// without these, a second process opening the file can hit
	// SQLITE_BUSY immediately instead of waiting briefly for the lock.
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: enable WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: set busy_timeout: %w", err)
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

type Agent struct {
	Name      string
	YAML      string
	CreatedAt int64
	UpdatedAt int64
}

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

// EnsureAgentExists creates a placeholder agent row if none exists yet, to
// satisfy runs.agent_name's foreign key when a run starts. Unlike
// UpsertAgent, it never overwrites an existing row — real agent config is
// owned by whoever called UpsertAgent (cli/api), and a run starting later
// must not clobber it back to a placeholder.
func (s *Store) EnsureAgentExists(ctx context.Context, name string) error {
	t := now()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agents (name, yaml, created_at, updated_at) VALUES (?, '', ?, ?)
		ON CONFLICT(name) DO NOTHING
	`, name, t, t)
	if err != nil {
		return fmt.Errorf("store: ensure agent %s exists: %w", name, err)
	}
	return nil
}

func (s *Store) GetAgent(ctx context.Context, name string) (*Agent, error) {
	var a Agent
	err := s.db.QueryRowContext(ctx, `
		SELECT name, yaml, created_at, updated_at FROM agents WHERE name = ?
	`, name).Scan(&a.Name, &a.YAML, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("store: agent %s: %w", name, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get agent %s: %w", name, err)
	}
	return &a, nil
}

func (s *Store) ListAgents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, yaml, created_at, updated_at FROM agents ORDER BY name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("store: list agents: %w", err)
	}
	defer rows.Close()

	var out []Agent
	for rows.Next() {
		var a Agent
		if err := rows.Scan(&a.Name, &a.YAML, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan agent: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAgent(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("store: delete agent %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete agent %s: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("store: agent %s: %w", name, ErrNotFound)
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
		return nil, fmt.Errorf("store: run %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get run %s: %w", id, err)
	}
	return &r, nil
}

// ListRuns returns runs ordered most-recent-first, using idx_runs_agent.
// An empty agentName returns runs across every agent. A non-positive
// limit returns every matching run; otherwise at most limit rows.
func (s *Store) ListRuns(ctx context.Context, agentName string, limit int) ([]Run, error) {
	query := `
		SELECT id, agent_name, state, turn_count, repair_count, error, created_at, updated_at
		FROM runs
	`
	var args []any
	if agentName != "" {
		query += ` WHERE agent_name = ?`
		args = append(args, agentName)
	}
	query += ` ORDER BY created_at DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()

	var out []Run
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.AgentName, &r.State, &r.TurnCount, &r.RepairCount, &r.Error, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
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
	DecidedBy  *string
	Reason     *string
	Result     *string
	IsError    bool
	CreatedAt  int64
	DecidedAt  *int64
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

// ListUnexecutedToolCalls returns tool calls whose approval has been
// settled (auto, approved, or denied) but haven't produced a result yet.
// Denied calls are included so the caller can turn them into an error
// tool_result without executing them; still-pending calls are excluded.
func (s *Store) ListUnexecutedToolCalls(ctx context.Context, runID string) ([]ToolCall, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, tool_name, args_json, approval, reason
		FROM tool_calls
		WHERE run_id = ? AND approval IN ('auto', 'approved', 'denied') AND result_json IS NULL
		ORDER BY created_at ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list unexecuted tool calls for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []ToolCall
	for rows.Next() {
		var tc ToolCall
		if err := rows.Scan(&tc.ID, &tc.RunID, &tc.ToolName, &tc.ArgsJSON, &tc.Approval, &tc.Reason); err != nil {
			return nil, fmt.Errorf("store: scan tool call: %w", err)
		}
		out = append(out, tc)
	}
	return out, rows.Err()
}

// ListPendingApprovals returns tool calls awaiting a human decision.
func (s *Store) ListPendingApprovals(ctx context.Context, runID string) ([]ToolCall, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, tool_name, args_json, approval
		FROM tool_calls WHERE run_id = ? AND approval = 'pending'
		ORDER BY created_at ASC
	`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list pending approvals for run %s: %w", runID, err)
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

// DecideToolCall records a human (or timeout-driven) decision on a call
// that is still pending. It fails with ErrNotPending if the call doesn't
// exist or was already decided, so a stale or duplicate decision doesn't
// silently overwrite an earlier one.
func (s *Store) DecideToolCall(ctx context.Context, id, decision, decidedBy, reason string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tool_calls SET approval = ?, decided_by = ?, reason = ?, decided_at = ?
		WHERE id = ? AND approval = 'pending'
	`, decision, decidedBy, reason, now(), id)
	if err != nil {
		return fmt.Errorf("store: decide tool call %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: decide tool call %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: tool call %s: %w", id, ErrNotPending)
	}
	return nil
}

// UpdateToolCallArgs overwrites a pending call's arguments (the chat REPL's
// "edit" action) without changing its approval state.
func (s *Store) UpdateToolCallArgs(ctx context.Context, id, argsJSON string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tool_calls SET args_json = ? WHERE id = ? AND approval = 'pending'
	`, argsJSON, id)
	if err != nil {
		return fmt.Errorf("store: update tool call args %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update tool call args %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: tool call %s: %w", id, ErrNotPending)
	}
	return nil
}

// CountPendingApprovals returns how many tool calls are still awaiting a
// decision for the run.
func (s *Store) CountPendingApprovals(ctx context.Context, runID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tool_calls WHERE run_id = ? AND approval = 'pending'
	`, runID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count pending approvals for run %s: %w", runID, err)
	}
	return n, nil
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
		SELECT id, run_id, tool_name, args_json, approval, decided_by, reason,
		       result_json, is_error, created_at, decided_at, executed_at
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
		if err := rows.Scan(&tc.ID, &tc.RunID, &tc.ToolName, &tc.ArgsJSON, &tc.Approval, &tc.DecidedBy, &tc.Reason,
			&tc.Result, &isErrorInt, &tc.CreatedAt, &tc.DecidedAt, &tc.ExecutedAt); err != nil {
			return nil, fmt.Errorf("store: scan tool call: %w", err)
		}
		tc.IsError = isErrorInt != 0
		out = append(out, tc)
	}
	return out, rows.Err()
}
