package postgres_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/noeljackson/symphony/go/internal/state"
	"github.com/noeljackson/symphony/go/internal/store/postgres"
)

const envURL = "SYMPHONY_TEST_POSTGRES_URL"

// openTestStore opens a clean PostgresStore against the URL at $envURL and
// resets the schema so the test starts from a known state. Skips the test
// (per SPEC §17.8 reporting rule) when the env var is unset.
func openTestStore(t *testing.T) *postgres.Store {
	t.Helper()
	url := os.Getenv(envURL)
	if url == "" {
		t.Skipf("set %s to run Postgres store tests", envURL)
	}
	store, err := postgres.Open(url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := wipeSchema(ctx, store.DB()); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store
}

func wipeSchema(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`DROP TABLE IF EXISTS symphony_recent_events`,
		`DROP TABLE IF EXISTS symphony_claimed`,
		`DROP TABLE IF EXISTS symphony_retries`,
		`DROP TABLE IF EXISTS symphony_agent_totals`,
		`DROP TABLE IF EXISTS symphony_meta`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func TestRestoreEmpty(t *testing.T) {
	s := openTestStore(t)
	snap, err := s.Restore(context.Background())
	if err != nil {
		t.Fatalf("restore empty: %v", err)
	}
	if snap.AgentTotals.CostUSD != nil {
		t.Fatal("expected nil CostUSD on empty store")
	}
	if len(snap.RetryAttempts) != 0 {
		t.Fatalf("expected zero retries, got %d", len(snap.RetryAttempts))
	}
	if len(snap.Claimed) != 0 {
		t.Fatalf("expected zero claimed, got %d", len(snap.Claimed))
	}
	if len(snap.RecentEventsByIssue) != 0 {
		t.Fatalf("expected zero recent_events buffers, got %d", len(snap.RecentEventsByIssue))
	}
}

func TestSaveAgentTotalsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	cost := 0.42
	costToday := 0.10
	day := time.Date(2026, 5, 3, 0, 0, 0, 0, time.UTC)
	pct := uint32(80)
	totals := state.AgentTotals{
		InputTokens: 1000, OutputTokens: 500, TotalTokens: 1500,
		SecondsRunning: 12.5, CostUSD: &cost, CostUSDToday: &costToday,
	}
	if err := s.SaveAgentTotals(ctx, totals, &day, &pct); err != nil {
		t.Fatalf("save: %v", err)
	}
	snap, err := s.Restore(ctx)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if snap.AgentTotals.InputTokens != 1000 {
		t.Fatalf("InputTokens: got %d want 1000", snap.AgentTotals.InputTokens)
	}
	if got := *snap.AgentTotals.CostUSD; got != 0.42 {
		t.Fatalf("CostUSD: got %v want 0.42", got)
	}
	if got := *snap.AgentTotals.CostUSDToday; got != 0.10 {
		t.Fatalf("CostUSDToday: got %v want 0.10", got)
	}
	if !snap.DailyCostWindow.Equal(day) {
		t.Fatalf("DailyCostWindow: got %v want %v", snap.DailyCostWindow, day)
	}
	if got := *snap.LastBudgetWarningPct; got != 80 {
		t.Fatalf("LastBudgetWarningPct: got %d want 80", got)
	}
}

func TestSaveAgentTotalsNullableFieldsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	totals := state.AgentTotals{InputTokens: 1, OutputTokens: 2, TotalTokens: 3, SecondsRunning: 4.0}
	if err := s.SaveAgentTotals(ctx, totals, nil, nil); err != nil {
		t.Fatalf("save: %v", err)
	}
	snap, err := s.Restore(ctx)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if snap.AgentTotals.CostUSD != nil {
		t.Fatal("CostUSD must round-trip as nil")
	}
	if snap.AgentTotals.CostUSDToday != nil {
		t.Fatal("CostUSDToday must round-trip as nil")
	}
	if snap.DailyCostWindow != nil {
		t.Fatal("DailyCostWindow must round-trip as nil")
	}
	if snap.LastBudgetWarningPct != nil {
		t.Fatal("LastBudgetWarningPct must round-trip as nil")
	}
}

func TestRetriesUpsertAndDelete(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	due := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	a := state.RetryEntry{IssueID: "id-a", Identifier: "MT-1", Attempt: 2, DueAt: due, Error: "boom"}
	if err := s.UpsertRetry(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	snap, _ := s.Restore(ctx)
	if got := snap.RetryAttempts["id-a"]; got == nil || got.Attempt != 2 {
		t.Fatalf("retry: got %+v want attempt=2", got)
	}
	// Upsert replaces.
	a.Attempt = 5
	a.Error = ""
	if err := s.UpsertRetry(ctx, a); err != nil {
		t.Fatalf("upsert replace: %v", err)
	}
	snap, _ = s.Restore(ctx)
	if got := snap.RetryAttempts["id-a"]; got.Attempt != 5 || got.Error != "" {
		t.Fatalf("retry replace: got %+v want attempt=5 error=\"\"", got)
	}
	// Delete clears.
	if err := s.DeleteRetry(ctx, "id-a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	snap, _ = s.Restore(ctx)
	if _, ok := snap.RetryAttempts["id-a"]; ok {
		t.Fatal("retry must be cleared after DeleteRetry")
	}
	// Deleting a missing entry is a no-op.
	if err := s.DeleteRetry(ctx, "missing"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestClaimedSetAndClear(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.SetClaimed(ctx, "id-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetClaimed(ctx, "id-b"); err != nil {
		t.Fatal(err)
	}
	// Set is idempotent.
	if err := s.SetClaimed(ctx, "id-a"); err != nil {
		t.Fatalf("idempotent set: %v", err)
	}
	snap, _ := s.Restore(ctx)
	if _, ok := snap.Claimed["id-a"]; !ok {
		t.Fatal("missing id-a in claimed")
	}
	if len(snap.Claimed) != 2 {
		t.Fatalf("claimed size: got %d want 2", len(snap.Claimed))
	}
	if err := s.ClearClaimed(ctx, "id-a"); err != nil {
		t.Fatal(err)
	}
	snap, _ = s.Restore(ctx)
	if _, ok := snap.Claimed["id-a"]; ok {
		t.Fatal("id-a must be cleared")
	}
	if len(snap.Claimed) != 1 {
		t.Fatalf("claimed size after clear: got %d want 1", len(snap.Claimed))
	}
}

func TestRecentEventsAppendAndCap(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < state.RecentEventsCap+5; i++ {
		ev := state.RecentEvent{
			At:      time.Date(2026, 5, 3, 12, 0, i, 0, time.UTC),
			Event:   "ev",
			Message: string(rune('a' + (i % 26))),
		}
		if err := s.AppendRecentEvent(ctx, "id-a", ev); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	snap, _ := s.Restore(ctx)
	got := snap.RecentEventsByIssue["id-a"]
	if len(got) != state.RecentEventsCap {
		t.Fatalf("recent_events length: got %d want %d", len(got), state.RecentEventsCap)
	}
	// Oldest 5 dropped → first surviving message corresponds to i=5.
	if got[0].Message != string(rune('a'+(5%26))) {
		t.Fatalf("oldest entry: got %q want corresponding to i=5", got[0].Message)
	}
}

func TestRecentEventsClearForIssue(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	ev := state.RecentEvent{At: time.Now().UTC(), Event: "x"}
	if err := s.AppendRecentEvent(ctx, "id-a", ev); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRecentEvent(ctx, "id-b", ev); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearRecentEventsForIssue(ctx, "id-a"); err != nil {
		t.Fatal(err)
	}
	snap, _ := s.Restore(ctx)
	if _, ok := snap.RecentEventsByIssue["id-a"]; ok {
		t.Fatal("id-a buffer must be cleared")
	}
	if len(snap.RecentEventsByIssue["id-b"]) != 1 {
		t.Fatalf("id-b survived clear: got %d want 1", len(snap.RecentEventsByIssue["id-b"]))
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// Already migrated by openTestStore. A second call should be a no-op.
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
}
