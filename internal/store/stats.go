package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Stats aggregates every run (optionally filtered to one agent) into the
// numbers `agentforge runs stats` reports: how often a run actually
// finishes cleanly, how much work it takes to get there, and what it
// costs. Every count here is already a column in schema.sql — this is a
// query layer over data runs already persist, not new tracking.
type Stats struct {
	TotalRuns     int
	CompletedRuns int
	FailedRuns    int
	CancelledRuns int
	// OtherRuns is TotalRuns minus the three above — runs still in
	// flight (ready_for_model, ready_for_tools) or awaiting_approval at
	// the moment Stats ran.
	OtherRuns int
	AvgTurns  float64

	TotalToolCalls  int
	FailedToolCalls int
	AvgToolCalls    float64

	InputTokens  int
	OutputTokens int
}

// SuccessRate is CompletedRuns/TotalRuns, or 0 for an empty Stats.
func (s Stats) SuccessRate() float64 {
	if s.TotalRuns == 0 {
		return 0
	}
	return float64(s.CompletedRuns) / float64(s.TotalRuns)
}

// ToolFailureRate is FailedToolCalls/TotalToolCalls, or 0 if no tool was
// ever called.
func (s Stats) ToolFailureRate() float64 {
	if s.TotalToolCalls == 0 {
		return 0
	}
	return float64(s.FailedToolCalls) / float64(s.TotalToolCalls)
}

// Stats aggregates every run for agentName, or every run in the store if
// agentName is "".
func (s *Store) Stats(ctx context.Context, agentName string) (*Stats, error) {
	var stats Stats

	runQuery := `
		SELECT COUNT(*), COALESCE(AVG(turn_count), 0),
		       SUM(CASE WHEN state = 'completed' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state = 'failed' THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state = 'cancelled' THEN 1 ELSE 0 END)
		FROM runs`
	var args []any
	if agentName != "" {
		runQuery += ` WHERE agent_name = ?`
		args = append(args, agentName)
	}
	var completed, failed, cancelled sql.NullInt64
	if err := s.db.QueryRowContext(ctx, runQuery, args...).Scan(
		&stats.TotalRuns, &stats.AvgTurns, &completed, &failed, &cancelled,
	); err != nil {
		return nil, fmt.Errorf("store: stats: %w", err)
	}
	stats.CompletedRuns, stats.FailedRuns, stats.CancelledRuns = int(completed.Int64), int(failed.Int64), int(cancelled.Int64)
	stats.OtherRuns = stats.TotalRuns - stats.CompletedRuns - stats.FailedRuns - stats.CancelledRuns

	toolQuery := `
		SELECT COUNT(*), SUM(CASE WHEN tc.is_error = 1 THEN 1 ELSE 0 END)
		FROM tool_calls tc JOIN runs r ON tc.run_id = r.id`
	toolArgs := args
	if agentName != "" {
		toolQuery += ` WHERE r.agent_name = ?`
	}
	var failedCalls sql.NullInt64
	if err := s.db.QueryRowContext(ctx, toolQuery, toolArgs...).Scan(&stats.TotalToolCalls, &failedCalls); err != nil {
		return nil, fmt.Errorf("store: stats: %w", err)
	}
	stats.FailedToolCalls = int(failedCalls.Int64)
	if stats.TotalRuns > 0 {
		stats.AvgToolCalls = float64(stats.TotalToolCalls) / float64(stats.TotalRuns)
	}

	tokenQuery := `
		SELECT COALESCE(SUM(m.input_tokens), 0), COALESCE(SUM(m.output_tokens), 0)
		FROM messages m JOIN runs r ON m.run_id = r.id`
	tokenArgs := args
	if agentName != "" {
		tokenQuery += ` WHERE r.agent_name = ?`
	}
	if err := s.db.QueryRowContext(ctx, tokenQuery, tokenArgs...).Scan(&stats.InputTokens, &stats.OutputTokens); err != nil {
		return nil, fmt.Errorf("store: stats: %w", err)
	}

	return &stats, nil
}
