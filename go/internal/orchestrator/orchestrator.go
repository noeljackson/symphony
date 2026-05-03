// Package orchestrator runs the SPEC §7 / §16 single-authority actor.
//
// Exactly one goroutine owns [state.OrchestratorState] and processes
// commands serially through an mpsc-style channel. Worker tasks, retry
// timers, and HTTP triggers all funnel through this channel — there's no
// shared mutable state to lock.
package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/noeljackson/symphony/go/internal/config"
	"github.com/noeljackson/symphony/go/internal/dispatch"
	"github.com/noeljackson/symphony/go/internal/issue"
	"github.com/noeljackson/symphony/go/internal/state"
	"github.com/noeljackson/symphony/go/internal/store"
	"github.com/noeljackson/symphony/go/internal/tracker"
)

// Command is the discriminated-union of messages the actor accepts.
//
// Tick / WorkerExit / AgentUpdate / RetryFire all originate from background
// goroutines (the poll loop, worker tasks, retry timers); Snapshot /
// Shutdown originate from the public Handle.
type Command interface{ commandTag() }

type cmdTick struct{}
type cmdWorkerExit struct {
	IssueID string
	Outcome WorkerOutcome
}
type cmdAgentUpdate struct {
	IssueID string
	Event   AgentEvent
}
type cmdRetryFire struct{ IssueID string }
type cmdSnapshot struct{ Reply chan<- Snapshot }
type cmdShutdown struct{}

func (cmdTick) commandTag()        {}
func (cmdWorkerExit) commandTag()  {}
func (cmdAgentUpdate) commandTag() {}
func (cmdRetryFire) commandTag()   {}
func (cmdSnapshot) commandTag()    {}
func (cmdShutdown) commandTag()    {}

// SnapshotRunningRow mirrors SPEC §13.7.2 `running[]`.
type SnapshotRunningRow struct {
	IssueID      string
	Identifier   string
	State        string
	SessionID    string
	TurnCount    uint32
	LastEvent    string
	LastMessage  string
	StartedAt    time.Time
	LastEventAt  *time.Time
	InputTokens  uint64
	OutputTokens uint64
	TotalTokens  uint64
	RecentEvents []state.RecentEvent
}

// SnapshotRetryRow mirrors SPEC §13.7.2 `retrying[]`.
type SnapshotRetryRow struct {
	IssueID    string
	Identifier string
	Attempt    uint32
	DueInMS    int64
	Error      string
}

// Snapshot is the synchronous-monitor view (SPEC §13.3 / §13.7.2).
type Snapshot struct {
	GeneratedAt time.Time
	Running     []SnapshotRunningRow
	Retrying    []SnapshotRetryRow
	AgentTotals state.AgentTotals
}

// Handle is the public surface for talking to the actor.
type Handle struct {
	cmd chan Command
}

// Tick triggers an immediate poll-and-dispatch cycle (SPEC §16.2).
func (h *Handle) Tick(ctx context.Context) {
	select {
	case h.cmd <- cmdTick{}:
	case <-ctx.Done():
	}
}

// AgentUpdate forwards one runtime event to the actor.
func (h *Handle) AgentUpdate(ctx context.Context, issueID string, ev AgentEvent) {
	select {
	case h.cmd <- cmdAgentUpdate{IssueID: issueID, Event: ev}:
	case <-ctx.Done():
	}
}

// Snapshot returns the current view.
func (h *Handle) Snapshot(ctx context.Context) (Snapshot, bool) {
	reply := make(chan Snapshot, 1)
	select {
	case h.cmd <- cmdSnapshot{Reply: reply}:
	case <-ctx.Done():
		return Snapshot{}, false
	}
	select {
	case snap := <-reply:
		return snap, true
	case <-ctx.Done():
		return Snapshot{}, false
	}
}

// Shutdown asks the actor to stop. Run returns once the goroutine has
// drained.
func (h *Handle) Shutdown(ctx context.Context) {
	select {
	case h.cmd <- cmdShutdown{}:
	case <-ctx.Done():
	}
}

// Orchestrator is the actor itself. Construct via [New], then call Run on
// the goroutine that should own the state.
type Orchestrator struct {
	cfg     *config.ServiceConfig
	state   *state.OrchestratorState
	tracker tracker.Tracker
	runner  WorkerRunner
	store   store.Store
	cmd     chan Command
	logger  *slog.Logger

	// auto-scheduled tick timer; cancelled on Shutdown.
	tickTimer *time.Timer

	// in-flight workers; their goroutines feed back via cmd.
	workersWG sync.WaitGroup

	// retry timers indexed by issue ID; cancelled on dispatch / shutdown.
	retryTimers map[string]*time.Timer

	// optional knob: when false, the actor does not self-schedule the next
	// tick. Tests want this off so they can drive ticks deterministically.
	autoSchedule bool

	// optional knob: skip the post-restart claimed-reconciliation step.
	// Defaults to false.
	skipRestartReconcile bool
}

// Options is the optional knobs accepted by [New].
type Options struct {
	Logger               *slog.Logger
	AutoSchedule         bool
	SkipRestartReconcile bool
}

// New constructs an orchestrator. The returned Handle is safe to use from
// any goroutine; the orchestrator itself is single-goroutine and must be
// Run on exactly one.
func New(cfg *config.ServiceConfig, tr tracker.Tracker, runner WorkerRunner, st store.Store, opts Options) (*Orchestrator, *Handle) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	o := &Orchestrator{
		cfg:                  cfg,
		state:                state.NewState(),
		tracker:              tr,
		runner:               runner,
		store:                st,
		cmd:                  make(chan Command, 256),
		logger:               logger,
		retryTimers:          map[string]*time.Timer{},
		autoSchedule:         opts.AutoSchedule,
		skipRestartReconcile: opts.SkipRestartReconcile,
	}
	o.state.PollIntervalMS = cfg.Polling.IntervalMS
	o.state.MaxConcurrentAgents = cfg.Agent.MaxConcurrentAgents
	return o, &Handle{cmd: o.cmd}
}

// Run blocks the caller until a Shutdown command is received or ctx is
// cancelled. Returns the final state for inspection.
//
// Workers receive a derived context that this method cancels on shutdown,
// so a runner that observes ctx.Done() unblocks promptly.
func (o *Orchestrator) Run(parent context.Context) *state.OrchestratorState {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if err := o.bootstrap(ctx); err != nil {
		o.logger.Warn("orchestrator bootstrap failed; continuing with empty state", slog.String("err", err.Error()))
	}
	for {
		select {
		case cmd := <-o.cmd:
			if _, ok := cmd.(cmdShutdown); ok {
				cancel()
				o.shutdown()
				return o.state
			}
			o.handle(ctx, cmd)
		case <-parent.Done():
			cancel()
			o.shutdown()
			return o.state
		}
	}
}

func (o *Orchestrator) bootstrap(ctx context.Context) error {
	snap, err := o.store.Restore(ctx)
	if err != nil {
		return fmt.Errorf("restore: %w", err)
	}
	if snap != nil {
		o.state.AgentTotals = snap.AgentTotals
		o.state.DailyCostWindow = snap.DailyCostWindow
		o.state.LastBudgetWarningPct = snap.LastBudgetWarningPct
		if snap.RetryAttempts != nil {
			o.state.RetryAttempts = snap.RetryAttempts
		}
		if snap.Claimed != nil {
			o.state.Claimed = snap.Claimed
		}
		// recent_events buffers are kept on each RunningEntry; we re-attach
		// them once the running entry is recreated post-reconcile.
	}

	// SPEC §13.5 lazy day rollover after a long shutdown.
	state.RollOverDailyCost(o.state, time.Now())

	if !o.skipRestartReconcile {
		o.reconcileClaimedAfterRestart(ctx)
	}

	// Re-arm any retries whose due_at is already in the past so they fire on
	// the first tick.
	now := time.Now()
	for _, retry := range o.state.RetryAttempts {
		o.armRetryTimer(retry, now)
	}
	return nil
}

func (o *Orchestrator) reconcileClaimedAfterRestart(ctx context.Context) {
	if len(o.state.Claimed) == 0 {
		return
	}
	ids := make([]string, 0, len(o.state.Claimed))
	for id := range o.state.Claimed {
		ids = append(ids, id)
	}
	current, err := o.tracker.FetchIssueStatesByIDs(ctx, ids)
	if err != nil {
		o.logger.Warn("post-restart tracker fetch failed; keeping claimed set as-is", slog.String("err", err.Error()))
		return
	}
	currentByID := map[string]issue.Issue{}
	for _, i := range current {
		currentByID[i.ID] = i
	}
	for id := range o.state.Claimed {
		i, ok := currentByID[id]
		if !ok {
			delete(o.state.Claimed, id)
			_ = o.store.ClearClaimed(ctx, id)
			continue
		}
		if isInList(o.cfg.Tracker.TerminalStates, i.State) {
			delete(o.state.Claimed, id)
			_ = o.store.ClearClaimed(ctx, id)
			delete(o.state.RetryAttempts, id)
			_ = o.store.DeleteRetry(ctx, id)
			continue
		}
		// Active but unowned by any in-process worker — re-queue as a retry
		// with a clear "process restart" marker.
		entry := o.state.RetryAttempts[id]
		nextAttempt := uint32(1)
		if entry != nil {
			nextAttempt = entry.Attempt + 1
		}
		retry := &state.RetryEntry{
			IssueID:    id,
			Identifier: i.Identifier,
			Attempt:    nextAttempt,
			DueAt:      time.Now(),
			Error:      "process restart",
		}
		o.state.RetryAttempts[id] = retry
		_ = o.store.UpsertRetry(ctx, *retry)
	}
}

func (o *Orchestrator) handle(ctx context.Context, cmd Command) {
	switch c := cmd.(type) {
	case cmdTick:
		o.runTick(ctx)
	case cmdAgentUpdate:
		o.applyAgentUpdate(ctx, c.IssueID, c.Event)
	case cmdWorkerExit:
		o.handleWorkerExit(ctx, c.IssueID, c.Outcome)
	case cmdRetryFire:
		o.handleRetryFire(ctx, c.IssueID)
	case cmdSnapshot:
		c.Reply <- o.snapshot()
	}
}

func (o *Orchestrator) runTick(ctx context.Context) {
	defer o.scheduleNextTick()

	state.RollOverDailyCost(o.state, time.Now())

	if err := o.cfg.ValidateForDispatch(); err != nil {
		o.logger.Warn("dispatch preflight failed", slog.String("err", err.Error()))
		return
	}

	if state.BudgetCapReached(o.state, o.cfg.Agent.DailyBudgetUSD) {
		o.maybeEmitBudgetWarnings()
		return
	}

	candidates, err := o.tracker.FetchCandidateIssues(ctx)
	if err != nil {
		o.logger.Warn("candidate fetch failed", slog.String("err", err.Error()))
		return
	}
	dispatch.SortForDispatch(candidates)
	for i := range candidates {
		issue := candidates[i]
		v := dispatch.Check(&issue, o.cfg, o.state)
		if v.Eligible {
			o.dispatch(ctx, issue, nil)
		} else if v.Reason == dispatch.VerdictGlobalSlotsExhausted {
			break
		}
	}
	o.maybeEmitBudgetWarnings()
}

func (o *Orchestrator) dispatch(ctx context.Context, i issue.Issue, attempt *uint32) {
	o.state.Claimed[i.ID] = struct{}{}
	_ = o.store.SetClaimed(ctx, i.ID)

	now := time.Now()
	entry := &state.RunningEntry{
		Identifier:      i.Identifier,
		Issue:           i,
		Session:         state.LiveSession{},
		RetryAttempt:    attempt,
		StartedAt:       now,
		StartedMonotone: now,
	}
	o.state.Running[i.ID] = entry

	events := make(chan AgentEvent, 64)
	o.workersWG.Add(1)
	go o.runWorker(ctx, i, attempt, events)
	go o.fanoutEvents(ctx, i.ID, events)
}

func (o *Orchestrator) runWorker(ctx context.Context, i issue.Issue, attempt *uint32, events chan AgentEvent) {
	defer o.workersWG.Done()
	outcome := o.runner.Run(ctx, i, attempt, events)
	close(events)
	select {
	case o.cmd <- cmdWorkerExit{IssueID: i.ID, Outcome: outcome}:
	case <-ctx.Done():
	}
}

func (o *Orchestrator) fanoutEvents(ctx context.Context, issueID string, events <-chan AgentEvent) {
	for ev := range events {
		select {
		case o.cmd <- cmdAgentUpdate{IssueID: issueID, Event: ev}:
		case <-ctx.Done():
			return
		}
	}
}

func (o *Orchestrator) applyAgentUpdate(ctx context.Context, issueID string, ev AgentEvent) {
	entry, ok := o.state.Running[issueID]
	if !ok {
		return
	}
	now := time.Now()
	entry.Session.LastAgentEvent = ev.Event
	entry.Session.LastAgentMessage = ev.Message
	entry.Session.LastAgentTimestamp = &now
	entry.Session.LastAgentTimestampMonotone = &now
	entry.Session.RecentEvents = state.PushRecentEvent(entry.Session.RecentEvents, state.RecentEvent{
		At:      now,
		Event:   ev.Event,
		Message: ev.Message,
	})
	_ = o.store.AppendRecentEvent(ctx, issueID, state.RecentEvent{
		At:      now,
		Event:   ev.Event,
		Message: ev.Message,
	})
}

func (o *Orchestrator) handleWorkerExit(ctx context.Context, issueID string, outcome WorkerOutcome) {
	entry, ok := o.state.Running[issueID]
	if !ok {
		return
	}
	delete(o.state.Running, issueID)
	delete(o.state.Claimed, issueID)
	_ = o.store.ClearClaimed(ctx, issueID)
	o.state.AgentTotals.SecondsRunning += time.Since(entry.StartedMonotone).Seconds()

	switch outcome.Kind {
	case WorkerOutcomeSuccess:
		// SPEC §8.4: continuation retry to give the tracker a chance to update.
		o.scheduleRetry(ctx, issueID, entry, outcome, true)
	case WorkerOutcomeFailure:
		o.scheduleRetry(ctx, issueID, entry, outcome, false)
	}
}

func (o *Orchestrator) scheduleRetry(ctx context.Context, issueID string, entry *state.RunningEntry, outcome WorkerOutcome, continuation bool) {
	previous := o.state.RetryAttempts[issueID]
	nextAttempt := uint32(1)
	if previous != nil {
		nextAttempt = previous.Attempt + 1
	}
	delay := dispatch.RetryDelay(nextAttempt, o.cfg.Agent.MaxRetryBackoffMS, continuation)
	dueAt := time.Now().Add(delay)
	retry := &state.RetryEntry{
		IssueID:    issueID,
		Identifier: entry.Identifier,
		Attempt:    nextAttempt,
		DueAt:      dueAt,
		Error:      outcome.Error,
	}
	o.state.RetryAttempts[issueID] = retry
	_ = o.store.UpsertRetry(ctx, *retry)
	o.armRetryTimer(retry, time.Now())
}

func (o *Orchestrator) armRetryTimer(retry *state.RetryEntry, now time.Time) {
	delay := time.Until(retry.DueAt)
	if delay < 0 {
		delay = 0
	}
	id := retry.IssueID
	if existing := o.retryTimers[id]; existing != nil {
		existing.Stop()
	}
	o.retryTimers[id] = time.AfterFunc(delay, func() {
		o.cmd <- cmdRetryFire{IssueID: id}
	})
	_ = now
}

func (o *Orchestrator) handleRetryFire(ctx context.Context, issueID string) {
	delete(o.retryTimers, issueID)
	retry, ok := o.state.RetryAttempts[issueID]
	if !ok {
		return
	}
	// Re-fetch the current state; if the issue went terminal while we waited,
	// drop the retry rather than re-dispatching.
	issues, err := o.tracker.FetchIssueStatesByIDs(ctx, []string{issueID})
	if err != nil {
		o.logger.Warn("retry-fire fetch failed; will retry on next backoff", slog.String("err", err.Error()))
		return
	}
	if len(issues) == 0 {
		delete(o.state.RetryAttempts, issueID)
		_ = o.store.DeleteRetry(ctx, issueID)
		return
	}
	current := issues[0]
	if isInList(o.cfg.Tracker.TerminalStates, current.State) {
		delete(o.state.RetryAttempts, issueID)
		_ = o.store.DeleteRetry(ctx, issueID)
		return
	}
	delete(o.state.RetryAttempts, issueID)
	_ = o.store.DeleteRetry(ctx, issueID)
	attempt := retry.Attempt
	o.dispatch(ctx, current, &attempt)
}

func (o *Orchestrator) maybeEmitBudgetWarnings() {
	cap := o.cfg.Agent.DailyBudgetUSD
	if cap == nil {
		return
	}
	if o.state.AgentTotals.CostUSDToday == nil {
		// Cap inert because pricing is unknown — one-shot warning per UTC day.
		if o.state.LastBudgetWarningPct == nil {
			o.logger.Warn("daily_budget_usd is set but the configured backend has no price-table entry; budget cap is inert",
				slog.Float64("cap_usd", *cap))
			zero := uint32(0)
			o.state.LastBudgetWarningPct = &zero
		}
		return
	}
	used := *o.state.AgentTotals.CostUSDToday
	pct := uint32(0)
	if *cap > 0 {
		pct = uint32((used / *cap) * 100.0)
	}
	already := uint32(0)
	if o.state.LastBudgetWarningPct != nil {
		already = *o.state.LastBudgetWarningPct
	}
	switch {
	case pct >= 100 && already < 100:
		o.logger.Warn("daily_budget_usd reached; new dispatches will be blocked until 00:00 UTC",
			slog.Float64("cap_usd", *cap), slog.Float64("used_usd", used))
		hundred := uint32(100)
		o.state.LastBudgetWarningPct = &hundred
	case pct >= 80 && already < 80:
		o.logger.Warn("daily_budget_usd at 80%",
			slog.Float64("cap_usd", *cap), slog.Float64("used_usd", used))
		eighty := uint32(80)
		o.state.LastBudgetWarningPct = &eighty
	}
}

func (o *Orchestrator) snapshot() Snapshot {
	now := time.Now().UTC()
	running := make([]SnapshotRunningRow, 0, len(o.state.Running))
	totals := o.state.AgentTotals
	for id, e := range o.state.Running {
		row := SnapshotRunningRow{
			IssueID:      id,
			Identifier:   e.Identifier,
			State:        e.Issue.State,
			SessionID:    e.Session.SessionID,
			TurnCount:    e.Session.TurnCount,
			LastEvent:    e.Session.LastAgentEvent,
			LastMessage:  e.Session.LastAgentMessage,
			StartedAt:    e.StartedAt,
			LastEventAt:  e.Session.LastAgentTimestamp,
			InputTokens:  e.Session.AgentInputTokens,
			OutputTokens: e.Session.AgentOutputTokens,
			TotalTokens:  e.Session.AgentTotalTokens,
			RecentEvents: append([]state.RecentEvent(nil), e.Session.RecentEvents...),
		}
		running = append(running, row)
		totals.SecondsRunning += time.Since(e.StartedMonotone).Seconds()
	}
	retrying := make([]SnapshotRetryRow, 0, len(o.state.RetryAttempts))
	for _, r := range o.state.RetryAttempts {
		retrying = append(retrying, SnapshotRetryRow{
			IssueID:    r.IssueID,
			Identifier: r.Identifier,
			Attempt:    r.Attempt,
			DueInMS:    int64(time.Until(r.DueAt) / time.Millisecond),
			Error:      r.Error,
		})
	}
	return Snapshot{
		GeneratedAt: now,
		Running:     running,
		Retrying:    retrying,
		AgentTotals: totals,
	}
}

func (o *Orchestrator) scheduleNextTick() {
	if !o.autoSchedule {
		return
	}
	if o.tickTimer != nil {
		o.tickTimer.Stop()
	}
	d := time.Duration(o.cfg.Polling.IntervalMS) * time.Millisecond
	o.tickTimer = time.AfterFunc(d, func() {
		select {
		case o.cmd <- cmdTick{}:
		default:
		}
	})
}

func (o *Orchestrator) shutdown() {
	if o.tickTimer != nil {
		o.tickTimer.Stop()
	}
	for _, t := range o.retryTimers {
		t.Stop()
	}
	o.workersWG.Wait()
}

func isInList(list []string, target string) bool {
	for _, s := range list {
		if strings.EqualFold(s, target) {
			return true
		}
	}
	return false
}
