package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations is the ordered list of schema-version transitions. Each entry's
// statements are executed in a single transaction. Position in the slice
// IS the schema version; never reorder, never delete.
var migrations = []string{
	// v1 — initial schema.
	`
	CREATE TABLE IF NOT EXISTS symphony_meta (
	  id INTEGER PRIMARY KEY,
	  schema_version INTEGER NOT NULL
	);
	INSERT INTO symphony_meta (id, schema_version)
	VALUES (1, 0)
	ON CONFLICT (id) DO NOTHING;

	CREATE TABLE IF NOT EXISTS symphony_agent_totals (
	  id INTEGER PRIMARY KEY,
	  input_tokens BIGINT NOT NULL DEFAULT 0,
	  output_tokens BIGINT NOT NULL DEFAULT 0,
	  total_tokens BIGINT NOT NULL DEFAULT 0,
	  seconds_running DOUBLE PRECISION NOT NULL DEFAULT 0,
	  cost_usd DOUBLE PRECISION,
	  cost_usd_today DOUBLE PRECISION,
	  daily_cost_window DATE,
	  last_budget_warning_pct INTEGER
	);

	CREATE TABLE IF NOT EXISTS symphony_retries (
	  issue_id TEXT PRIMARY KEY,
	  identifier TEXT NOT NULL,
	  attempt INTEGER NOT NULL,
	  due_at TIMESTAMPTZ NOT NULL,
	  error TEXT
	);
	CREATE INDEX IF NOT EXISTS symphony_retries_due_at_idx
	  ON symphony_retries (due_at);

	CREATE TABLE IF NOT EXISTS symphony_claimed (
	  issue_id TEXT PRIMARY KEY
	);

	CREATE TABLE IF NOT EXISTS symphony_recent_events (
	  issue_id TEXT NOT NULL,
	  seq BIGINT NOT NULL,
	  at TIMESTAMPTZ NOT NULL,
	  event TEXT NOT NULL,
	  message TEXT,
	  PRIMARY KEY (issue_id, seq)
	);
	`,
}

// Migrate brings the schema up to the latest known version. Each missing
// version is applied inside a transaction; partial application is rolled
// back so reruns either advance the version or leave it unchanged.
//
// Implementations MUST refuse to start when the on-disk schema_version is
// AHEAD of len(migrations) — that means a newer Symphony binary wrote the
// schema and rolling back would lose data. The error is intentionally
// fatal.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS symphony_meta (
		  id INTEGER PRIMARY KEY,
		  schema_version INTEGER NOT NULL
		);`); err != nil {
		return fmt.Errorf("create symphony_meta: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO symphony_meta (id, schema_version)
		VALUES (1, 0)
		ON CONFLICT (id) DO NOTHING;`); err != nil {
		return fmt.Errorf("seed symphony_meta: %w", err)
	}

	current, err := s.currentVersion(ctx)
	if err != nil {
		return err
	}
	if current > len(migrations) {
		return fmt.Errorf(
			"symphony state store schema version %d is newer than this binary's %d; refusing to start",
			current, len(migrations),
		)
	}
	for i := current; i < len(migrations); i++ {
		if err := s.applyMigration(ctx, i+1, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}

func (s *Store) currentVersion(ctx context.Context) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT schema_version FROM symphony_meta WHERE id = 1`)
	var v int
	if err := row.Scan(&v); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, fmt.Errorf("read schema_version: %w", err)
	}
	return v, nil
}

func (s *Store) applyMigration(ctx context.Context, target int, sqlText string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, sqlText); err != nil {
		return fmt.Errorf("exec migration sql: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE symphony_meta SET schema_version = $1 WHERE id = 1`,
		target); err != nil {
		return fmt.Errorf("bump schema_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
