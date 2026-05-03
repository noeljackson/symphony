// Package store defines the persistent state contract per SPEC §4.1.9.
//
// Implementations (Postgres for production, in-memory for tests) hold cost
// ledger, retry queue, recent_events ring buffer, and the claimed-set
// across orchestrator restarts. The interface is deliberately narrow so
// implementations can use whatever transactional primitives their backend
// provides.
package store

import (
	"context"
	"time"

	"github.com/noeljackson/symphony/go/internal/state"
)

// Snapshot is the persisted subset of OrchestratorState. Fields that aren't
// persisted (Running, AgentRateLimits, runtime knobs) are absent here.
type Snapshot struct {
	AgentTotals          state.AgentTotals
	DailyCostWindow      *time.Time
	LastBudgetWarningPct *uint32
	RetryAttempts        map[string]*state.RetryEntry
	Claimed              map[string]struct{}
	RecentEventsByIssue  map[string][]state.RecentEvent
}

// Store is the SPEC §4.1.9 persistence contract.
//
// Implementations MUST treat all writes that touch the cost ledger or
// warning suppressor atomically (the cost-add + warning-state pair never
// half-commits). Reads (Restore) MAY be non-atomic with respect to
// concurrent writes; the orchestrator only Restores at boot.
type Store interface {
	// Restore returns the persisted snapshot. When the store is empty (fresh
	// install / never written to), implementations return a zero-valued
	// Snapshot and nil error so the caller can proceed with §16.1 defaults.
	Restore(ctx context.Context) (*Snapshot, error)

	// SaveAgentTotals persists the cost ledger + daily-window state +
	// warning suppressor as one atomic update.
	SaveAgentTotals(ctx context.Context, totals state.AgentTotals, window *time.Time, warningPct *uint32) error

	// UpsertRetry persists or replaces one queued retry entry.
	UpsertRetry(ctx context.Context, entry state.RetryEntry) error

	// DeleteRetry clears the persisted retry for an issue. No-op when the
	// issue has no entry.
	DeleteRetry(ctx context.Context, issueID string) error

	// SetClaimed and ClearClaimed mutate the persisted claimed-set. Used to
	// detect crashed workers on restart per §4.1.9.
	SetClaimed(ctx context.Context, issueID string) error
	ClearClaimed(ctx context.Context, issueID string) error

	// AppendRecentEvent appends one entry to the per-issue ring buffer.
	// Implementations MUST cap each issue's buffer at RecentEventsCap by
	// dropping the oldest entry; the SPEC §13.7.2 cap is enforced server-side.
	AppendRecentEvent(ctx context.Context, issueID string, ev state.RecentEvent) error

	// ClearRecentEventsForIssue drops the per-issue buffer when a session
	// ends or the issue exits the running set.
	ClearRecentEventsForIssue(ctx context.Context, issueID string) error
}
