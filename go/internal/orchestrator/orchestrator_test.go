package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/state"
	"github.com/noeljackson/symphony/go/internal/store"
	"github.com/noeljackson/symphony/go/internal/tracker"
)

type scriptedRunner struct {
	mu         sync.Mutex
	dispatched []string
	outcomes   []WorkerOutcome
	gateAfter  int
	gateOnce   sync.Once
	gate       chan struct{}
}

func newScriptedRunner(outcomes []WorkerOutcome) *scriptedRunner {
	return &scriptedRunner{outcomes: outcomes, gate: make(chan struct{})}
}

func (r *scriptedRunner) armGate() {
	// Workers block on `gate` until released, simulating a long-running session.
}

func (r *scriptedRunner) release() {
	r.gateOnce.Do(func() { close(r.gate) })
}

func (r *scriptedRunner) Run(ctx context.Context, i issue.Issue, _ *uint32, events chan<- AgentEvent) WorkerOutcome {
	r.mu.Lock()
	r.dispatched = append(r.dispatched, i.Identifier)
	pop := WorkerOutcome{Kind: WorkerOutcomeSuccess}
	if len(r.outcomes) > 0 {
		pop = r.outcomes[0]
		r.outcomes = r.outcomes[1:]
	}
	r.mu.Unlock()

	// Emit one synthetic event so applyAgentUpdate has something to record.
	select {
	case events <- AgentEvent{Event: "session_started", Message: i.Identifier}:
	case <-ctx.Done():
		return WorkerOutcome{Kind: WorkerOutcomeFailure, Error: "ctx cancelled"}
	}

	select {
	case <-r.gate:
	case <-ctx.Done():
		return WorkerOutcome{Kind: WorkerOutcomeFailure, Error: "ctx cancelled"}
	}
	return pop
}

func (r *scriptedRunner) dispatchedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.dispatched)
}

func (r *scriptedRunner) dispatchedNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.dispatched...)
}

func makeConfig(maxConcurrent int) *config.ServiceConfig {
	return &config.ServiceConfig{
		Tracker: config.TrackerConfig{
			Kind:           config.TrackerLinear,
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Done", "Cancelled"},
		},
		Linear:  config.LinearConfig{Endpoint: "https://x", APIKey: "k", ProjectSlug: "demo"},
		Polling: config.PollingConfig{IntervalMS: 30_000},
		Agent: config.AgentConfig{
			Backend:                    config.BackendCodex,
			MaxConcurrentAgents:        maxConcurrent,
			MaxRetryBackoffMS:          300_000,
			MaxConcurrentAgentsByState: map[string]int{},
		},
		Codex: config.CodexConfig{Command: "true"},
	}
}

func mkIssue(id, ident, st string) issue.Issue {
	return issue.Issue{ID: id, Identifier: ident, Title: "t", State: st}
}

func bootActor(t *testing.T, cfg *config.ServiceConfig, tr tracker.Tracker, runner WorkerRunner) (*Handle, *Orchestrator, func()) {
	t.Helper()
	silentLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	o, h := New(cfg, tr, runner, store.NewMemoryStore(), Options{Logger: silentLogger})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()
	return h, o, func() {
		h.Shutdown(ctx)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			cancel()
			<-done
		}
		cancel()
	}
}

func waitForCondition(t *testing.T, label string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for: %s", label)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOrchestratorDispatchesAllEligibleIssues(t *testing.T) {
	// Sort-order semantics are unit-tested in dispatch_test.go; this test
	// just confirms the actor walks the sorted candidate list and starts a
	// worker for each one.
	cfg := makeConfig(10)
	highPrio := 1
	lowPrio := 3
	high := mkIssue("a", "MT-1", "Todo")
	high.Priority = &highPrio
	low := mkIssue("b", "MT-2", "Todo")
	low.Priority = &lowPrio
	tr := tracker.NewMemoryTracker([]issue.Issue{low, high})
	runner := newScriptedRunner(nil)
	defer runner.release()

	h, _, shutdown := bootActor(t, cfg, tr, runner)
	defer shutdown()

	h.Tick(context.Background())
	waitForCondition(t, "two dispatches", func() bool { return runner.dispatchedCount() == 2 })
	got := runner.dispatchedNames()
	seen := map[string]bool{}
	for _, n := range got {
		seen[n] = true
	}
	if !seen["MT-1"] || !seen["MT-2"] {
		t.Fatalf("dispatched: got %v want both MT-1 and MT-2", got)
	}
}

func TestOrchestratorGlobalConcurrencyCapLimitsDispatch(t *testing.T) {
	cfg := makeConfig(1)
	issues := []issue.Issue{
		mkIssue("a", "MT-1", "Todo"),
		mkIssue("b", "MT-2", "Todo"),
		mkIssue("c", "MT-3", "Todo"),
	}
	tr := tracker.NewMemoryTracker(issues)
	runner := newScriptedRunner(nil)
	defer runner.release()

	h, _, shutdown := bootActor(t, cfg, tr, runner)
	defer shutdown()

	h.Tick(context.Background())
	waitForCondition(t, "first dispatch", func() bool { return runner.dispatchedCount() == 1 })
	time.Sleep(50 * time.Millisecond)
	if got := runner.dispatchedCount(); got != 1 {
		t.Fatalf("dispatch count under cap=1: got %d want 1", got)
	}
}

func TestOrchestratorSnapshotReportsRunningEntry(t *testing.T) {
	cfg := makeConfig(10)
	tr := tracker.NewMemoryTracker([]issue.Issue{mkIssue("a", "MT-1", "Todo")})
	runner := newScriptedRunner(nil)
	defer runner.release()

	h, _, shutdown := bootActor(t, cfg, tr, runner)
	defer shutdown()

	h.Tick(context.Background())
	waitForCondition(t, "running entry", func() bool {
		snap, ok := h.Snapshot(context.Background())
		return ok && len(snap.Running) == 1
	})

	// Wait for the synthetic event to be applied so recent_events shows up.
	var snap Snapshot
	waitForCondition(t, "recent_events populated", func() bool {
		var ok bool
		snap, ok = h.Snapshot(context.Background())
		return ok && len(snap.Running) == 1 && len(snap.Running[0].RecentEvents) > 0
	})
	if snap.Running[0].Identifier != "MT-1" {
		t.Fatalf("running identifier: got %q want MT-1", snap.Running[0].Identifier)
	}
	if got := snap.Running[0].RecentEvents[0].Event; got != "session_started" {
		t.Fatalf("recent_events[0].Event: got %q want session_started", got)
	}
}

func TestOrchestratorBudgetCapBlocksNewDispatches(t *testing.T) {
	cfg := makeConfig(10)
	cap := 1.0
	cfg.Agent.DailyBudgetUSD = &cap
	tr := tracker.NewMemoryTracker([]issue.Issue{mkIssue("a", "MT-1", "Todo")})
	runner := newScriptedRunner(nil)
	defer runner.release()

	// Seed the store: cost_usd_today >= cap. Bootstrap reads from the store,
	// so any state we want present at first tick has to live there.
	memStore := store.NewMemoryStore()
	day := time.Now().UTC()
	day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	used := 1.5
	if err := memStore.SaveAgentTotals(context.Background(),
		state.AgentTotals{CostUSD: &used, CostUSDToday: &used},
		&day, nil); err != nil {
		t.Fatalf("seed store: %v", err)
	}

	silentLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	o, h := New(cfg, tr, runner, memStore, Options{Logger: silentLogger})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()
	defer func() {
		h.Shutdown(ctx)
		<-done
		cancel()
	}()

	h.Tick(context.Background())
	time.Sleep(75 * time.Millisecond)
	if got := runner.dispatchedCount(); got != 0 {
		t.Fatalf("expected zero dispatches with cap reached, got %d", got)
	}
}

func TestRecentEventsSurviveRestartViaStore(t *testing.T) {
	cfg := makeConfig(10)
	tr := tracker.NewMemoryTracker([]issue.Issue{mkIssue("a", "MT-1", "Todo")})
	runner := newScriptedRunner(nil)
	defer runner.release()

	memStore := store.NewMemoryStore()
	silentLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	o, h := New(cfg, tr, runner, memStore, Options{Logger: silentLogger})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		o.Run(ctx)
		close(done)
	}()

	h.Tick(context.Background())
	waitForCondition(t, "recent_events written to store", func() bool {
		snap, _ := memStore.Restore(ctx)
		return snap != nil && len(snap.RecentEventsByIssue["a"]) > 0
	})
	h.Shutdown(ctx)
	<-done
	cancel()

	persisted, _ := memStore.Restore(context.Background())
	if got := len(persisted.RecentEventsByIssue["a"]); got == 0 {
		t.Fatalf("expected persisted recent_events for issue 'a', got %d", got)
	}
	// Sanity: the synthetic event we emitted is in the buffer.
	found := false
	for _, ev := range persisted.RecentEventsByIssue["a"] {
		if ev.Event == "session_started" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected session_started in persisted recent_events")
	}
}
