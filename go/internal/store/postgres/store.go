// Package postgres provides a PostgreSQL-backed implementation of [store.Store]
// per SPEC v3 §4.1.9.
//
// Each PostgresStore instance owns one workflow's persistent state. The
// schema is laid out as scalar tables (one row per concept) since v3 core
// conformance assumes a single orchestrator process per (workflow, store).
// Multi-workflow tenancy is out of scope.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/noeljackson/symphony/go/internal/state"
	"github.com/noeljackson/symphony/go/internal/store"
)

// Store is a PostgresStore. Construct via [Open].
type Store struct {
	db *sql.DB
}

// Open returns a PostgresStore connected at databaseURL. The caller owns
// the underlying *sql.DB lifecycle via [Store.DB] and [Store.Close].
//
// Open does not run migrations; call [Store.Migrate] to bring the schema
// to the current version before issuing reads or writes.
func Open(databaseURL string) (*Store, error) {
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	return &Store{db: db}, nil
}

// New wraps an existing *sql.DB. Useful when the caller manages the
// connection pool centrally (e.g. for sharing with other Postgres consumers
// in the same process).
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// DB returns the underlying *sql.DB.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the underlying connection pool.
func (s *Store) Close() error { return s.db.Close() }

// Restore implements [store.Store].
func (s *Store) Restore(ctx context.Context) (*store.Snapshot, error) {
	out := &store.Snapshot{
		RetryAttempts:       map[string]*state.RetryEntry{},
		Claimed:             map[string]struct{}{},
		RecentEventsByIssue: map[string][]state.RecentEvent{},
	}
	if err := s.loadAgentTotals(ctx, out); err != nil {
		return nil, err
	}
	if err := s.loadRetries(ctx, out); err != nil {
		return nil, err
	}
	if err := s.loadClaimed(ctx, out); err != nil {
		return nil, err
	}
	if err := s.loadRecentEvents(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) loadAgentTotals(ctx context.Context, out *store.Snapshot) error {
	row := s.db.QueryRowContext(ctx, `
		SELECT input_tokens, output_tokens, total_tokens, seconds_running,
		       cost_usd, cost_usd_today, daily_cost_window, last_budget_warning_pct
		FROM symphony_agent_totals WHERE id = 1`)
	var in, outT, totT int64
	var sec float64
	var costUSD, costToday sql.NullFloat64
	var window sql.NullTime
	var warnPct sql.NullInt32
	err := row.Scan(&in, &outT, &totT, &sec, &costUSD, &costToday, &window, &warnPct)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("scan agent_totals: %w", err)
	}
	out.AgentTotals = state.AgentTotals{
		InputTokens:    uint64(in),
		OutputTokens:   uint64(outT),
		TotalTokens:    uint64(totT),
		SecondsRunning: sec,
	}
	if costUSD.Valid {
		v := costUSD.Float64
		out.AgentTotals.CostUSD = &v
	}
	if costToday.Valid {
		v := costToday.Float64
		out.AgentTotals.CostUSDToday = &v
	}
	if window.Valid {
		t := window.Time.UTC()
		out.DailyCostWindow = &t
	}
	if warnPct.Valid {
		v := uint32(warnPct.Int32)
		out.LastBudgetWarningPct = &v
	}
	return nil
}

func (s *Store) loadRetries(ctx context.Context, out *store.Snapshot) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT issue_id, identifier, attempt, due_at, error
		FROM symphony_retries`)
	if err != nil {
		return fmt.Errorf("query retries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, ident string
			attempt   int
			due       time.Time
			errStr    sql.NullString
		)
		if err := rows.Scan(&id, &ident, &attempt, &due, &errStr); err != nil {
			return fmt.Errorf("scan retry: %w", err)
		}
		entry := &state.RetryEntry{
			IssueID:    id,
			Identifier: ident,
			Attempt:    uint32(attempt),
			DueAt:      due,
		}
		if errStr.Valid {
			entry.Error = errStr.String
		}
		out.RetryAttempts[id] = entry
	}
	return rows.Err()
}

func (s *Store) loadClaimed(ctx context.Context, out *store.Snapshot) error {
	rows, err := s.db.QueryContext(ctx, `SELECT issue_id FROM symphony_claimed`)
	if err != nil {
		return fmt.Errorf("query claimed: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan claimed: %w", err)
		}
		out.Claimed[id] = struct{}{}
	}
	return rows.Err()
}

func (s *Store) loadRecentEvents(ctx context.Context, out *store.Snapshot) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT issue_id, seq, at, event, message
		FROM symphony_recent_events
		ORDER BY issue_id, seq`)
	if err != nil {
		return fmt.Errorf("query recent_events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id      string
			seq     int64
			at      time.Time
			ev      string
			message sql.NullString
		)
		if err := rows.Scan(&id, &seq, &at, &ev, &message); err != nil {
			return fmt.Errorf("scan recent_event: %w", err)
		}
		entry := state.RecentEvent{At: at, Event: ev}
		if message.Valid {
			entry.Message = message.String
		}
		out.RecentEventsByIssue[id] = append(out.RecentEventsByIssue[id], entry)
	}
	return rows.Err()
}

// SaveAgentTotals implements [store.Store].
//
// Atomicity: cost-add + warning-state + window date all commit together so
// a crash mid-update can't leave the daily warning to fire twice or lose
// the cost delta.
func (s *Store) SaveAgentTotals(ctx context.Context, totals state.AgentTotals, window *time.Time, warningPct *uint32) error {
	costUSD := nullableFloat(totals.CostUSD)
	costToday := nullableFloat(totals.CostUSDToday)
	wndw := nullableTime(window)
	warn := nullableInt32(warningPct)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO symphony_agent_totals
		  (id, input_tokens, output_tokens, total_tokens, seconds_running,
		   cost_usd, cost_usd_today, daily_cost_window, last_budget_warning_pct)
		VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
		  input_tokens = EXCLUDED.input_tokens,
		  output_tokens = EXCLUDED.output_tokens,
		  total_tokens = EXCLUDED.total_tokens,
		  seconds_running = EXCLUDED.seconds_running,
		  cost_usd = EXCLUDED.cost_usd,
		  cost_usd_today = EXCLUDED.cost_usd_today,
		  daily_cost_window = EXCLUDED.daily_cost_window,
		  last_budget_warning_pct = EXCLUDED.last_budget_warning_pct`,
		int64(totals.InputTokens), int64(totals.OutputTokens), int64(totals.TotalTokens),
		totals.SecondsRunning, costUSD, costToday, wndw, warn)
	if err != nil {
		return fmt.Errorf("save agent_totals: %w", err)
	}
	return nil
}

// UpsertRetry implements [store.Store].
func (s *Store) UpsertRetry(ctx context.Context, entry state.RetryEntry) error {
	errStr := sql.NullString{Valid: entry.Error != "", String: entry.Error}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO symphony_retries (issue_id, identifier, attempt, due_at, error)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (issue_id) DO UPDATE SET
		  identifier = EXCLUDED.identifier,
		  attempt = EXCLUDED.attempt,
		  due_at = EXCLUDED.due_at,
		  error = EXCLUDED.error`,
		entry.IssueID, entry.Identifier, int(entry.Attempt), entry.DueAt, errStr)
	if err != nil {
		return fmt.Errorf("upsert retry: %w", err)
	}
	return nil
}

// DeleteRetry implements [store.Store]. No-op when the issue has no entry.
func (s *Store) DeleteRetry(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM symphony_retries WHERE issue_id = $1`, issueID)
	if err != nil {
		return fmt.Errorf("delete retry: %w", err)
	}
	return nil
}

// SetClaimed implements [store.Store].
func (s *Store) SetClaimed(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO symphony_claimed (issue_id) VALUES ($1)
		ON CONFLICT (issue_id) DO NOTHING`, issueID)
	if err != nil {
		return fmt.Errorf("set claimed: %w", err)
	}
	return nil
}

// ClearClaimed implements [store.Store].
func (s *Store) ClearClaimed(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM symphony_claimed WHERE issue_id = $1`, issueID)
	if err != nil {
		return fmt.Errorf("clear claimed: %w", err)
	}
	return nil
}

// AppendRecentEvent implements [store.Store]. The cap is enforced server-side
// per SPEC §13.7.2 by deleting the oldest entries that exceed it.
func (s *Store) AppendRecentEvent(ctx context.Context, issueID string, ev state.RecentEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var nextSeq int64
	err = tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(seq), 0) + 1
		FROM symphony_recent_events WHERE issue_id = $1`, issueID).Scan(&nextSeq)
	if err != nil {
		return fmt.Errorf("compute seq: %w", err)
	}

	message := sql.NullString{Valid: ev.Message != "", String: ev.Message}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO symphony_recent_events (issue_id, seq, at, event, message)
		VALUES ($1, $2, $3, $4, $5)`,
		issueID, nextSeq, ev.At, ev.Event, message); err != nil {
		return fmt.Errorf("insert recent_event: %w", err)
	}

	// Trim to RECENT_EVENTS_CAP by deleting the oldest entries.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM symphony_recent_events
		WHERE issue_id = $1
		  AND seq <= (
		    SELECT seq FROM (
		      SELECT seq, ROW_NUMBER() OVER (ORDER BY seq DESC) AS rn
		      FROM symphony_recent_events WHERE issue_id = $1
		    ) sub WHERE sub.rn = $2
		  ) - 1`,
		issueID, state.RecentEventsCap+1); err != nil {
		return fmt.Errorf("trim recent_events: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit recent_event: %w", err)
	}
	return nil
}

// ClearRecentEventsForIssue implements [store.Store].
func (s *Store) ClearRecentEventsForIssue(ctx context.Context, issueID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM symphony_recent_events WHERE issue_id = $1`, issueID)
	if err != nil {
		return fmt.Errorf("clear recent_events: %w", err)
	}
	return nil
}

// nullableFloat / nullableTime / nullableInt32 mirror SPEC v3's nullability
// guarantees: a nil pointer becomes SQL NULL; a non-nil pointer becomes the
// dereferenced value.
func nullableFloat(p *float64) sql.NullFloat64 {
	if p == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Valid: true, Float64: *p}
}

func nullableTime(p *time.Time) sql.NullTime {
	if p == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Valid: true, Time: *p}
}

func nullableInt32(p *uint32) sql.NullInt32 {
	if p == nil {
		return sql.NullInt32{}
	}
	return sql.NullInt32{Valid: true, Int32: int32(*p)}
}
