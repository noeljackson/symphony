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

// cmdReload swaps the orchestrator's config and (optionally) its tracker
// and worker runner per SPEC §6.2. The actor cfg-swap covers polling
// cadence, concurrency caps, budget cap, retry backoff, terminal_states
// and per-backend stall_timeout_ms; the optional Components struct
// covers tracker auth, agent.backend, per-backend command/endpoint,
// workspace.root, and hooks (whose constructors otherwise close over
// the old values at boot time).
//
// In-flight workers continue on the runner active at their dispatch
// time; only new dispatches pick up the swapped runner. SPEC §6.2:
// "Implementations are not REQUIRED to restart in-flight agent sessions
// automatically when config changes."
type cmdReload struct {
	Definition *config.WorkflowDefinition
	Components ReloadComponents
	Reply      chan<- ReloadOutcome
}
type cmdShutdown struct{}

// ReloadComponents holds rebuilt runtime dependencies that should be
// swapped atomically along with the cfg pointer. A nil field means
// "keep the existing component". The caller is responsible for
// reconstructing only the components whose backing config fields
// actually changed — see cmd/symphony for the diff-and-rebuild logic.
type ReloadComponents struct {
	Tracker tracker.Tracker
	Runner  WorkerRunner
}

// ReloadOutcome is the reply the actor sends back after handling cmdReload.
// SPEC §6.2: invalid reloads MUST NOT crash; the orchestrator keeps the
// last-known-good config and the caller logs the err.
type ReloadOutcome struct {
	Applied        bool
	Err            error
	Restart        bool     // set when restart-required fields changed AND Components didn't supply a swap.
	Affected       []string // list of keys that DID hot-swap (for operator-visible logs).
	SwappedTracker bool     // true when Components.Tracker replaced o.tracker.
	SwappedRunner  bool     // true when Components.Runner replaced o.runner.
}

func (cmdTick) commandTag()        {}
func (cmdWorkerExit) commandTag()  {}
func (cmdReload) commandTag()      {}
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

// detectStalls implements SPEC §8.5 Part A. For each running entry whose
// elapsed-since-last-event exceeds the configured stall_timeout_ms, mark
// the issue as stalled and cancel its worker context. The worker observes
// ctx.Done() and returns; handleWorkerExit then converts to a retry with
// RetryEntry.error == "stall_timeout".
//
// Looks up the per-backend `stall_timeout_ms`. When <= 0 stall detection
// is disabled (SPEC §5.3.6).
func (o *Orchestrator) detectStalls() {
	stallMS := o.stallTimeoutMS()
	if stallMS <= 0 {
		return
	}
	stall := time.Duration(stallMS) * time.Millisecond
	now := time.Now()
	for id, entry := range o.state.Running {
		if _, already := o.stalledIDs[id]; already {
			continue
		}
		var since time.Time
		if entry.Session.LastAgentTimestampMonotone != nil {
			since = *entry.Session.LastAgentTimestampMonotone
		} else {
			since = entry.StartedMonotone
		}
		if now.Sub(since) <= stall {
			continue
		}
		o.logger.Warn("stall detected",
			slog.String("identifier", entry.Identifier),
			slog.Duration("elapsed", now.Sub(since)),
			slog.Duration("limit", stall))
		o.stalledIDs[id] = struct{}{}
		if cancel, ok := o.workerCancels[id]; ok {
			cancel()
		}
	}
}

// stallTimeoutMS returns the per-backend stall timeout. When the
// configured backend's value is unset or zero, the SPEC §5.3.6 default
// of 300_000ms applies.
func (o *Orchestrator) stallTimeoutMS() int64 {
	switch o.cfg.Agent.Backend {
	case config.BackendCodex:
		return o.cfg.Codex.StallTimeoutMS
	case config.BackendClaudeCode:
		return o.cfg.ClaudeCode.StallTimeoutMS
	}
	// HTTP backends (openai_compat, anthropic_messages) don't expose a
	// stall_timeout knob in v1 — the per-attempt turn_timeout is enough.
	return 0
}

// EventBroadcast is one envelope sent on each [Handle.SubscribeEvents] channel
// when the actor records a new agent update.
type EventBroadcast struct {
	IssueID    string
	Identifier string
	Event      AgentEvent
}

// EventChannelCapacity is the per-subscriber buffer depth. Subscribers that
// fall behind by more than this number of events get drops on the floor —
// callers SHOULD re-snapshot to recover (SPEC §13.7.4).
const EventChannelCapacity = 64

// stallTimeoutSentinel is the SPEC §8.5 RetryEntry.error value the
// orchestrator writes when stall detection cancels a worker. Keeping it
// stable means observability surfaces (dashboard, symphony logs,
// /api/v1/<id>.retry) can match against a single string.
const stallTimeoutSentinel = "stall_timeout"

// Handle is the public surface for talking to the actor.
type Handle struct {
	cmd chan Command

	subsMu sync.Mutex
	subs   map[chan EventBroadcast]struct{}
}

// SubscribeEvents registers a new subscriber. The returned channel buffers
// up to [EventChannelCapacity] events; further events are dropped. Callers
// MUST invoke the returned cancel function to release the subscription —
// closing it leaks otherwise.
func (h *Handle) SubscribeEvents() (<-chan EventBroadcast, func()) {
	ch := make(chan EventBroadcast, EventChannelCapacity)
	h.subsMu.Lock()
	if h.subs == nil {
		h.subs = map[chan EventBroadcast]struct{}{}
	}
	h.subs[ch] = struct{}{}
	h.subsMu.Unlock()
	return ch, func() {
		h.subsMu.Lock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
		h.subsMu.Unlock()
	}
}

// broadcast fans `ev` out to every active subscriber. Slow subscribers get
// the event dropped (non-blocking send) so the actor never blocks on
// observability.
func (h *Handle) broadcast(ev EventBroadcast) {
	h.subsMu.Lock()
	defer h.subsMu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// drop — subscriber is too slow
		}
	}
}

// closeAllSubscribers releases every active subscription. Called once on
// orchestrator shutdown so HTTP handlers see the channel close.
func (h *Handle) closeAllSubscribers() {
	h.subsMu.Lock()
	defer h.subsMu.Unlock()
	for ch := range h.subs {
		close(ch)
		delete(h.subs, ch)
	}
}

// Tick triggers an immediate poll-and-dispatch cycle (SPEC §16.2).
func (h *Handle) Tick(ctx context.Context) {
	select {
	case h.cmd <- cmdTick{}:
	case <-ctx.Done():
	}
}

// Reload asks the actor to swap its config (and optionally its tracker
// and runner) per SPEC §6.2. Components fields that are non-nil replace
// the corresponding orchestrator dependency; nil fields are left as-is.
// The returned ReloadOutcome reports which keys hot-swapped, whether
// any restart-required keys still need a process restart (Components
// did not supply a fresh tracker/runner for them), and any preflight
// error. If ctx is cancelled before the actor replies, an outcome with
// Err set is returned.
func (h *Handle) Reload(ctx context.Context, def *config.WorkflowDefinition, components ReloadComponents) ReloadOutcome {
	reply := make(chan ReloadOutcome, 1)
	select {
	case h.cmd <- cmdReload{Definition: def, Components: components, Reply: reply}:
	case <-ctx.Done():
		return ReloadOutcome{Err: ctx.Err()}
	}
	select {
	case out := <-reply:
		return out
	case <-ctx.Done():
		return ReloadOutcome{Err: ctx.Err()}
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
	handle  *Handle // shared between the actor and external callers
	logger  *slog.Logger

	// auto-scheduled tick timer; cancelled on Shutdown.
	tickTimer *time.Timer

	// in-flight workers; their goroutines feed back via cmd.
	workersWG sync.WaitGroup

	// per-worker context cancel funcs, indexed by issue ID. Used by
	// SPEC §8.5 stall detection to cancel a stuck attempt; cleaned up
	// in handleWorkerExit.
	workerCancels map[string]context.CancelFunc

	// SPEC §8.5: when stall detection cancels a worker, the issue is
	// added here so handleWorkerExit knows to override the retry's
	// error to the sentinel "stall_timeout" regardless of what the
	// runner actually returned.
	stalledIDs map[string]struct{}

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
	cmd := make(chan Command, 256)
	h := &Handle{cmd: cmd}
	o := &Orchestrator{
		cfg:                  cfg,
		state:                state.NewState(),
		tracker:              tr,
		runner:               runner,
		store:                st,
		cmd:                  cmd,
		handle:               h,
		logger:               logger,
		retryTimers:          map[string]*time.Timer{},
		workerCancels:        map[string]context.CancelFunc{},
		stalledIDs:           map[string]struct{}{},
		autoSchedule:         opts.AutoSchedule,
		skipRestartReconcile: opts.SkipRestartReconcile,
	}
	o.state.PollIntervalMS = cfg.Polling.IntervalMS
	o.state.MaxConcurrentAgents = cfg.Agent.MaxConcurrentAgents
	return o, h
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
			o.handleCommand(ctx, cmd)
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

func (o *Orchestrator) handleCommand(ctx context.Context, cmd Command) {
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
	case cmdReload:
		c.Reply <- o.handleReload(c.Definition, c.Components)
	}
}

// handleReload swaps the actor's *config.ServiceConfig to the validated
// one in def, mirrors the changed values on OrchestratorState, and
// optionally swaps the tracker / runner if the caller pre-rebuilt them
// in components. The outcome lists which keys hot-swapped, which
// components were swapped, and whether restart-required keys remain
// (Restart=true means at least one restart-required field changed and
// the caller did NOT supply a rebuilt component for it).
func (o *Orchestrator) handleReload(def *config.WorkflowDefinition, components ReloadComponents) ReloadOutcome {
	if def == nil {
		return ReloadOutcome{Err: fmt.Errorf("nil workflow definition")}
	}
	newCfg := def.Config
	if err := newCfg.ValidateForDispatch(); err != nil {
		return ReloadOutcome{Err: fmt.Errorf("validate: %w", err)}
	}
	affected, restart := diffConfig(o.cfg, newCfg)

	// Swap the cfg pointer atomically from the actor's perspective —
	// nothing else races against o.cfg because every read happens on
	// this goroutine.
	o.cfg = newCfg
	o.state.PollIntervalMS = newCfg.Polling.IntervalMS
	o.state.MaxConcurrentAgents = newCfg.Agent.MaxConcurrentAgents

	swappedTracker := false
	if components.Tracker != nil {
		o.tracker = components.Tracker
		swappedTracker = true
	}
	swappedRunner := false
	if components.Runner != nil {
		o.runner = components.Runner
		swappedRunner = true
	}

	// Restart=true reports the leftover gap: restart-required fields
	// changed AND the caller didn't ship a component swap that would
	// satisfy them. When the caller rebuilt both tracker and runner,
	// every restart-required field is now serviceable in-process.
	restartGap := len(restart) > 0 && !(swappedTracker && swappedRunner)

	if len(affected) > 0 || len(restart) > 0 {
		o.logger.Info("workflow reload applied",
			slog.Any("hot_swapped", affected),
			slog.Any("restart_required", restart),
			slog.Bool("swapped_tracker", swappedTracker),
			slog.Bool("swapped_runner", swappedRunner))
	}
	return ReloadOutcome{
		Applied:        true,
		Affected:       affected,
		Restart:        restartGap,
		SwappedTracker: swappedTracker,
		SwappedRunner:  swappedRunner,
	}
}

// diffConfig returns (hotSwapped, restartRequired) — keys that are
// observably different between the old and new config, split by whether
// the field can take effect via a cfg-pointer swap or whether it needs
// a process restart because some constructor closed over the old value.
func diffConfig(oldCfg, newCfg *config.ServiceConfig) (hot, restart []string) {
	if oldCfg == nil || newCfg == nil {
		return nil, nil
	}
	// Hot-swappable: orchestrator reads these from o.cfg every tick.
	if oldCfg.Polling.IntervalMS != newCfg.Polling.IntervalMS {
		hot = append(hot, "polling.interval_ms")
	}
	if oldCfg.Agent.MaxConcurrentAgents != newCfg.Agent.MaxConcurrentAgents {
		hot = append(hot, "agent.max_concurrent_agents")
	}
	if oldCfg.Agent.MaxRetryBackoffMS != newCfg.Agent.MaxRetryBackoffMS {
		hot = append(hot, "agent.max_retry_backoff_ms")
	}
	if !floatPtrEq(oldCfg.Agent.DailyBudgetUSD, newCfg.Agent.DailyBudgetUSD) {
		hot = append(hot, "agent.daily_budget_usd")
	}
	if !mapStringIntEq(oldCfg.Agent.MaxConcurrentAgentsByState, newCfg.Agent.MaxConcurrentAgentsByState) {
		hot = append(hot, "agent.max_concurrent_agents_by_state")
	}
	if !sliceStringEq(oldCfg.Tracker.TerminalStates, newCfg.Tracker.TerminalStates) {
		hot = append(hot, "tracker.terminal_states")
	}
	if oldCfg.Codex.StallTimeoutMS != newCfg.Codex.StallTimeoutMS {
		hot = append(hot, "codex.stall_timeout_ms")
	}
	if oldCfg.ClaudeCode.StallTimeoutMS != newCfg.ClaudeCode.StallTimeoutMS {
		hot = append(hot, "claude_code.stall_timeout_ms")
	}
	// Restart-required: tracker / backend / workspace / hooks / server
	// constructors closed over the old values at boot time.
	if oldCfg.Tracker.Kind != newCfg.Tracker.Kind {
		restart = append(restart, "tracker.kind")
	}
	if !sliceStringEq(oldCfg.Tracker.ActiveStates, newCfg.Tracker.ActiveStates) {
		restart = append(restart, "tracker.active_states")
	}
	if oldCfg.Linear != newCfg.Linear {
		restart = append(restart, "linear.*")
	}
	if !githubEq(oldCfg.GitHub, newCfg.GitHub) {
		restart = append(restart, "github.*")
	}
	if oldCfg.Agent.Backend != newCfg.Agent.Backend {
		restart = append(restart, "agent.backend")
	}
	if oldCfg.Agent.MaxTurns != newCfg.Agent.MaxTurns {
		restart = append(restart, "agent.max_turns")
	}
	if !codexEq(oldCfg.Codex, newCfg.Codex) {
		restart = append(restart, "codex.*")
	}
	if !claudeCodeEq(oldCfg.ClaudeCode, newCfg.ClaudeCode) {
		restart = append(restart, "claude_code.*")
	}
	if oldCfg.OpenAICompat != newCfg.OpenAICompat {
		restart = append(restart, "openai_compat.*")
	}
	if oldCfg.AnthropicMessages != newCfg.AnthropicMessages {
		restart = append(restart, "anthropic_messages.*")
	}
	if oldCfg.Workspace.Root != newCfg.Workspace.Root {
		restart = append(restart, "workspace.root")
	}
	if !hooksEq(oldCfg.Hooks, newCfg.Hooks) {
		restart = append(restart, "hooks.*")
	}
	if !uint16PtrEq(oldCfg.Server.Port, newCfg.Server.Port) {
		restart = append(restart, "server.port")
	}
	return hot, restart
}

func floatPtrEq(a, b *float64) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func uint16PtrEq(a, b *uint16) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func strPtrEq(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return *a == *b
	}
}

func sliceStringEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mapStringIntEq(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

func githubEq(a, b config.GitHubConfig) bool {
	if a.Endpoint != b.Endpoint || a.Owner != b.Owner || a.Repo != b.Repo ||
		a.APIToken != b.APIToken || a.AppID != b.AppID ||
		a.AppInstallationID != b.AppInstallationID || a.PrivateKey != b.PrivateKey ||
		a.Assignee != b.Assignee {
		return false
	}
	return mapStringIntEq(a.LabelPriorityMap, b.LabelPriorityMap)
}

func codexEq(a, b config.CodexConfig) bool {
	// ApprovalPolicy / ThreadSandbox / TurnSandboxPolicy are `any`; treat
	// them as restart-required iff scalar fields differ. The Raw pass-
	// through values flow through cfg.Codex directly so a deep compare via
	// reflection is overkill here — operators who change those will see
	// differing scalar fields too in practice.
	return a.Command == b.Command &&
		a.TurnTimeoutMS == b.TurnTimeoutMS &&
		a.ReadTimeoutMS == b.ReadTimeoutMS &&
		a.StallTimeoutMS == b.StallTimeoutMS
}

func claudeCodeEq(a, b config.ClaudeCodeConfig) bool {
	if a.Command != b.Command || a.PermissionMode != b.PermissionMode ||
		a.Model != b.Model || a.TurnTimeoutMS != b.TurnTimeoutMS ||
		a.ReadTimeoutMS != b.ReadTimeoutMS || a.StallTimeoutMS != b.StallTimeoutMS {
		return false
	}
	return sliceStringEq(a.AllowedTools, b.AllowedTools) &&
		sliceStringEq(a.DisallowedTools, b.DisallowedTools)
}

func hooksEq(a, b config.HooksConfig) bool {
	return strPtrEq(a.AfterCreate, b.AfterCreate) &&
		strPtrEq(a.BeforeRun, b.BeforeRun) &&
		strPtrEq(a.AfterRun, b.AfterRun) &&
		strPtrEq(a.BeforeRemove, b.BeforeRemove) &&
		a.TimeoutMS == b.TimeoutMS
}

func (o *Orchestrator) runTick(ctx context.Context) {
	defer o.scheduleNextTick()

	state.RollOverDailyCost(o.state, time.Now())

	// SPEC §8.5 Part A: stall detection runs at the head of every tick
	// so a stuck worker is cancelled before we consider new dispatches.
	o.detectStalls()

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

	// SPEC §8.5: each worker gets its own derived context so stall
	// detection can cancel it without tearing down the whole actor.
	workerCtx, cancel := context.WithCancel(ctx)
	o.workerCancels[i.ID] = cancel

	events := make(chan AgentEvent, 64)
	o.workersWG.Add(1)
	// Capture o.runner here on the actor goroutine so a concurrent
	// hot-swap of o.runner via cmdReload can't race the worker's read.
	// In-flight workers continue on the runner that was active at
	// dispatch time; new dispatches pick up the swapped runner.
	runner := o.runner
	go o.runWorker(workerCtx, runner, i, attempt, events)
	go o.fanoutEvents(workerCtx, i.ID, events)
}

func (o *Orchestrator) runWorker(ctx context.Context, runner WorkerRunner, i issue.Issue, attempt *uint32, events chan AgentEvent) {
	defer o.workersWG.Done()
	outcome := runner.Run(ctx, i, attempt, events)
	close(events)
	// Always try to deliver cmdWorkerExit so the actor can clean up
	// state and schedule a retry. Watching ctx.Done() here is incorrect:
	// when SPEC §8.5 stall detection cancels the per-worker ctx, ctx is
	// already Done before we get here, and a select racing the channel
	// send against ctx.Done() will sometimes drop the exit on the floor.
	// The 5s timeout is the shutdown-deadlock guard: if the actor's run
	// loop has already returned, no one drains cmd, and we'd block
	// forever otherwise.
	select {
	case o.cmd <- cmdWorkerExit{IssueID: i.ID, Outcome: outcome}:
	case <-time.After(5 * time.Second):
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
	// SPEC §13.7.4: fan out to SSE subscribers. Non-blocking — slow
	// subscribers see drops rather than blocking the actor.
	o.handle.broadcast(EventBroadcast{
		IssueID:    issueID,
		Identifier: entry.Identifier,
		Event:      ev,
	})
}

func (o *Orchestrator) handleWorkerExit(ctx context.Context, issueID string, outcome WorkerOutcome) {
	entry, ok := o.state.Running[issueID]
	if !ok {
		return
	}
	// Release the per-worker cancel func; future stall-detect ticks
	// must not see a stale entry.
	if cancel, ok := o.workerCancels[issueID]; ok {
		cancel()
		delete(o.workerCancels, issueID)
	}
	// SPEC §8.5: if this exit was driven by stall-detection
	// cancellation, override the outcome's error to the sentinel
	// regardless of what the runner actually returned. This gives
	// operators a stable signal in /api/v1/<id>.retry and recent_events.
	if _, stalled := o.stalledIDs[issueID]; stalled {
		outcome.Kind = WorkerOutcomeFailure
		outcome.Error = stallTimeoutSentinel
		delete(o.stalledIDs, issueID)
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
	o.handle.closeAllSubscribers()
}

func isInList(list []string, target string) bool {
	for _, s := range list {
		if strings.EqualFold(s, target) {
			return true
		}
	}
	return false
}
