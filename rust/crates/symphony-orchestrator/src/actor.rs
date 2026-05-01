//! The orchestrator actor. Single-task ownership of [`OrchestratorState`].
//!
//! Commands are processed sequentially so we never have to worry about
//! concurrent state mutation or duplicate dispatch (SPEC §7.4).

use std::collections::HashMap;
use std::sync::Arc;
use std::time::{Duration, Instant};

use symphony_codex::events::RuntimeEvent;
use symphony_core::config::ServiceConfig;
use symphony_core::Issue;
use symphony_tracker::{IssueState, Tracker};
use time::OffsetDateTime;
use tokio::sync::{mpsc, oneshot};
use tokio::task::JoinHandle;

use crate::dispatch::{
    dispatch_eligibility, retry_delay_ms, sort_for_dispatch, EligibilityVerdict,
};
use crate::state::{LiveSession, OrchestratorState, RetryEntry, RunningEntry};
use crate::worker::{WorkerOutcome, WorkerRunner};
use crate::workspace_cleaner::{NoopCleaner, WorkspaceCleaner};

/// Commands accepted by the orchestrator actor. Workers, retry timers, the
/// poll loop, and HTTP triggers all funnel through this single channel.
#[derive(Debug)]
pub enum OrchestratorCommand {
    Tick,
    /// `RefreshNow` is best-effort: if a tick is already running we just
    /// signal the reply channel.
    RefreshNow {
        reply: oneshot::Sender<()>,
    },
    WorkerExit {
        issue_id: String,
        outcome: WorkerOutcome,
    },
    CodexUpdate {
        issue_id: String,
        event: Box<RuntimeEvent>,
    },
    RetryFire {
        issue_id: String,
    },
    ConfigReload(Arc<ServiceConfig>),
    Snapshot {
        reply: oneshot::Sender<Snapshot>,
    },
    Shutdown,
}

#[derive(Debug, Clone)]
pub struct SnapshotRunningRow {
    pub issue_id: String,
    pub identifier: String,
    pub state: String,
    pub session_id: Option<String>,
    pub turn_count: u32,
    pub last_event: Option<String>,
    pub last_message: Option<String>,
    pub started_at: OffsetDateTime,
    pub last_event_at: Option<OffsetDateTime>,
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
}

#[derive(Debug, Clone)]
pub struct SnapshotRetryRow {
    pub issue_id: String,
    pub identifier: String,
    pub attempt: u32,
    pub due_in_ms: i64,
    pub error: Option<String>,
}

#[derive(Debug, Clone)]
pub struct Snapshot {
    pub generated_at: OffsetDateTime,
    pub running: Vec<SnapshotRunningRow>,
    pub retrying: Vec<SnapshotRetryRow>,
    pub codex_totals: crate::state::CodexTotals,
}

/// Public handle the rest of the system uses to talk to the orchestrator.
#[derive(Clone)]
pub struct OrchestratorHandle {
    cmd_tx: mpsc::Sender<OrchestratorCommand>,
}

impl OrchestratorHandle {
    pub async fn tick(&self) {
        let _ = self.cmd_tx.send(OrchestratorCommand::Tick).await;
    }

    pub async fn refresh_now(&self) -> bool {
        let (tx, rx) = oneshot::channel();
        if self
            .cmd_tx
            .send(OrchestratorCommand::RefreshNow { reply: tx })
            .await
            .is_err()
        {
            return false;
        }
        rx.await.is_ok()
    }

    pub async fn snapshot(&self) -> Option<Snapshot> {
        let (tx, rx) = oneshot::channel();
        self.cmd_tx
            .send(OrchestratorCommand::Snapshot { reply: tx })
            .await
            .ok()?;
        rx.await.ok()
    }

    pub async fn reload(&self, cfg: Arc<ServiceConfig>) {
        let _ = self
            .cmd_tx
            .send(OrchestratorCommand::ConfigReload(cfg))
            .await;
    }

    pub async fn shutdown(&self) {
        let _ = self.cmd_tx.send(OrchestratorCommand::Shutdown).await;
    }

    pub fn raw_sender(&self) -> mpsc::Sender<OrchestratorCommand> {
        self.cmd_tx.clone()
    }
}

pub struct Orchestrator {
    cfg: Arc<ServiceConfig>,
    state: OrchestratorState,
    tracker: Arc<dyn Tracker>,
    runner: Arc<dyn WorkerRunner>,
    cmd_rx: mpsc::Receiver<OrchestratorCommand>,
    cmd_tx: mpsc::Sender<OrchestratorCommand>,
    worker_tasks: HashMap<String, JoinHandle<()>>,
    retry_tasks: HashMap<String, JoinHandle<()>>,
    pending_refresh_replies: Vec<oneshot::Sender<()>>,
    scheduled_tick: Option<JoinHandle<()>>,
    auto_schedule: bool,
    cleaner: Arc<dyn WorkspaceCleaner>,
}

impl Orchestrator {
    pub fn new(
        cfg: Arc<ServiceConfig>,
        tracker: Arc<dyn Tracker>,
        runner: Arc<dyn WorkerRunner>,
    ) -> (Self, OrchestratorHandle) {
        let (cmd_tx, cmd_rx) = mpsc::channel(256);
        let state = OrchestratorState {
            poll_interval_ms: cfg.polling.interval_ms,
            max_concurrent_agents: cfg.agent.max_concurrent_agents,
            ..Default::default()
        };
        let actor = Orchestrator {
            cfg,
            state,
            tracker,
            runner,
            cmd_rx,
            cmd_tx: cmd_tx.clone(),
            worker_tasks: HashMap::new(),
            retry_tasks: HashMap::new(),
            pending_refresh_replies: Vec::new(),
            scheduled_tick: None,
            auto_schedule: false,
            cleaner: Arc::new(NoopCleaner),
        };
        let handle = OrchestratorHandle { cmd_tx };
        (actor, handle)
    }

    /// Plug in a workspace cleaner so terminal reconciliation removes the
    /// per-issue directory (SPEC §8.5 terminal branch).
    pub fn with_cleaner(mut self, cleaner: Arc<dyn WorkspaceCleaner>) -> Self {
        self.cleaner = cleaner;
        self
    }

    /// Enable self-scheduling: after every tick the actor schedules the next
    /// tick `polling.interval_ms` later. The first tick must still be
    /// triggered by the caller (e.g. `handle.tick().await`).
    pub fn with_auto_schedule(mut self, enable: bool) -> Self {
        self.auto_schedule = enable;
        self
    }

    /// Drive the actor until [`OrchestratorCommand::Shutdown`] is received.
    pub async fn run(mut self) -> OrchestratorState {
        while let Some(cmd) = self.cmd_rx.recv().await {
            if matches!(cmd, OrchestratorCommand::Shutdown) {
                self.shutdown().await;
                break;
            }
            self.handle(cmd).await;
        }
        self.state
    }

    async fn handle(&mut self, cmd: OrchestratorCommand) {
        match cmd {
            OrchestratorCommand::Tick => self.run_tick().await,
            OrchestratorCommand::RefreshNow { reply } => {
                self.pending_refresh_replies.push(reply);
                self.run_tick().await;
            }
            OrchestratorCommand::WorkerExit { issue_id, outcome } => {
                self.handle_worker_exit(issue_id, outcome).await;
            }
            OrchestratorCommand::CodexUpdate { issue_id, event } => {
                self.apply_codex_update(&issue_id, *event);
            }
            OrchestratorCommand::RetryFire { issue_id } => {
                self.handle_retry_fire(&issue_id).await;
            }
            OrchestratorCommand::ConfigReload(cfg) => {
                self.cfg = cfg;
                self.state.poll_interval_ms = self.cfg.polling.interval_ms;
                self.state.max_concurrent_agents = self.cfg.agent.max_concurrent_agents;
            }
            OrchestratorCommand::Snapshot { reply } => {
                let _ = reply.send(self.snapshot());
            }
            OrchestratorCommand::Shutdown => {
                // handled by run()
            }
        }
    }

    async fn run_tick(&mut self) {
        // SPEC §16.2: reconcile -> validate preflight -> fetch -> sort ->
        // dispatch.
        self.reconcile().await;

        if let Err(e) = self.cfg.validate_for_dispatch() {
            tracing::warn!(error = %e, "dispatch preflight failed");
            self.flush_refresh_replies();
            return;
        }

        let mut candidates = match self.tracker.fetch_candidate_issues().await {
            Ok(v) => v,
            Err(e) => {
                tracing::warn!(error = %e, "candidate fetch failed");
                self.flush_refresh_replies();
                return;
            }
        };
        sort_for_dispatch(&mut candidates);
        for issue in candidates {
            let verdict = dispatch_eligibility(&issue, &self.cfg, &self.state);
            if verdict.eligible {
                self.dispatch(issue, None);
            } else if matches!(verdict.reason, EligibilityVerdict::GlobalSlotsExhausted) {
                break;
            }
        }
        self.flush_refresh_replies();
        self.schedule_next_tick();
    }

    fn schedule_next_tick(&mut self) {
        if !self.auto_schedule {
            return;
        }
        if let Some(handle) = self.scheduled_tick.take() {
            handle.abort();
        }
        let cmd_tx = self.cmd_tx.clone();
        let interval = Duration::from_millis(self.cfg.polling.interval_ms);
        self.scheduled_tick = Some(tokio::spawn(async move {
            tokio::time::sleep(interval).await;
            let _ = cmd_tx.send(OrchestratorCommand::Tick).await;
        }));
    }

    fn flush_refresh_replies(&mut self) {
        for reply in self.pending_refresh_replies.drain(..) {
            let _ = reply.send(());
        }
    }

    async fn reconcile(&mut self) {
        // Part A: stall detection.
        if self.cfg.codex.stall_timeout_ms > 0 {
            let now_mono = Instant::now();
            let stall = Duration::from_millis(self.cfg.codex.stall_timeout_ms as u64);
            let stalled: Vec<String> = self
                .state
                .running
                .iter()
                .filter_map(|(id, entry)| {
                    let last = entry
                        .session
                        .last_codex_timestamp_monotonic
                        .unwrap_or(entry.started_monotonic);
                    if now_mono.duration_since(last) > stall {
                        Some(id.clone())
                    } else {
                        None
                    }
                })
                .collect();
            for id in stalled {
                tracing::warn!(issue_id = %id, "stall detected, terminating worker");
                self.terminate_running(&id, false, "stall_timeout").await;
            }
        }

        // Part B: tracker state refresh.
        let running_ids: Vec<String> = self.state.running.keys().cloned().collect();
        if running_ids.is_empty() {
            return;
        }
        let refreshed = match self.tracker.fetch_issue_states_by_ids(&running_ids).await {
            Ok(v) => v,
            Err(e) => {
                tracing::debug!(error = %e, "reconcile state refresh failed; keeping workers running");
                return;
            }
        };
        let by_id: HashMap<String, IssueState> =
            refreshed.into_iter().map(|s| (s.id.clone(), s)).collect();

        for id in running_ids {
            match by_id.get(&id) {
                Some(state) => {
                    let in_terminal = self
                        .cfg
                        .tracker
                        .terminal_states
                        .iter()
                        .any(|s| s.eq_ignore_ascii_case(&state.state));
                    let in_active = self
                        .cfg
                        .tracker
                        .active_states
                        .iter()
                        .any(|s| s.eq_ignore_ascii_case(&state.state));
                    if in_terminal {
                        self.terminate_running(&id, true, "tracker_terminal").await;
                    } else if in_active {
                        if let Some(entry) = self.state.running.get_mut(&id) {
                            entry.issue.state = state.state.clone();
                        }
                    } else {
                        self.terminate_running(&id, false, "non_active_state").await;
                    }
                }
                None => {
                    // Issue vanished from the tracker — leave the worker
                    // alone for now; SPEC §8.5 says reconcile failure keeps
                    // workers running.
                }
            }
        }
    }

    async fn terminate_running(&mut self, issue_id: &str, cleanup_workspace: bool, reason: &str) {
        if let Some(handle) = self.worker_tasks.remove(issue_id) {
            handle.abort();
        }
        let entry = match self.state.running.remove(issue_id) {
            Some(e) => e,
            None => return,
        };
        self.update_runtime_total(&entry);
        if cleanup_workspace {
            self.cleaner.remove(&entry.identifier).await;
        }
        tracing::info!(issue_id = %issue_id, reason = %reason, "worker terminated by orchestrator");
        // Cancellation does not auto-retry; SPEC §8.5 tracker-state-refresh
        // path either drops the claim or re-queues on the next tick.
        self.state.claimed.remove(issue_id);
        self.cancel_retry_timer(issue_id);
    }

    async fn handle_worker_exit(&mut self, issue_id: String, outcome: WorkerOutcome) {
        self.worker_tasks.remove(&issue_id);
        let entry = match self.state.running.remove(&issue_id) {
            Some(e) => e,
            None => return,
        };
        self.update_runtime_total(&entry);
        match outcome {
            WorkerOutcome::Success => {
                self.state.completed.insert(issue_id.clone());
                self.schedule_retry(
                    issue_id,
                    entry.identifier.clone(),
                    1,
                    None,
                    /*continuation=*/ true,
                );
            }
            WorkerOutcome::Failure { error } => {
                let next_attempt = entry.retry_attempt.unwrap_or(0).saturating_add(1).max(1);
                self.schedule_retry(
                    issue_id,
                    entry.identifier.clone(),
                    next_attempt,
                    Some(error),
                    /*continuation=*/ false,
                );
            }
            WorkerOutcome::Cancelled { reason: _ } => {
                self.state.claimed.remove(&issue_id);
            }
        }
    }

    fn dispatch(&mut self, issue: Issue, attempt: Option<u32>) {
        let issue_id = issue.id.clone();
        let identifier = issue.identifier.clone();
        let runner = self.runner.clone();
        let cmd_tx = self.cmd_tx.clone();
        let issue_for_task = issue.clone();
        let attempt_for_task = attempt;
        let id_for_event = issue_id.clone();
        let id_for_exit = issue_id.clone();

        let (codex_tx, mut codex_rx) = mpsc::channel(64);
        let event_pump_tx = cmd_tx.clone();
        let pump = tokio::spawn(async move {
            while let Some(event) = codex_rx.recv().await {
                let _ = event_pump_tx
                    .send(OrchestratorCommand::CodexUpdate {
                        issue_id: id_for_event.clone(),
                        event: Box::new(event),
                    })
                    .await;
            }
        });

        let task = tokio::spawn(async move {
            let outcome = runner.run(issue_for_task, attempt_for_task, codex_tx).await;
            // Allow the pump task to drain remaining events before signalling.
            drop(pump);
            let _ = cmd_tx
                .send(OrchestratorCommand::WorkerExit {
                    issue_id: id_for_exit,
                    outcome,
                })
                .await;
        });

        self.worker_tasks.insert(issue_id.clone(), task);
        self.cancel_retry_timer(&issue_id);
        self.state.claimed.insert(issue_id.clone());
        self.state.retry_attempts.remove(&issue_id);
        self.state.running.insert(
            issue_id,
            RunningEntry {
                identifier,
                issue,
                session: LiveSession::default(),
                retry_attempt: attempt,
                started_at: OffsetDateTime::now_utc(),
                started_monotonic: Instant::now(),
            },
        );
    }

    fn schedule_retry(
        &mut self,
        issue_id: String,
        identifier: String,
        attempt: u32,
        error: Option<String>,
        continuation: bool,
    ) {
        let delay_ms = retry_delay_ms(attempt, self.cfg.agent.max_retry_backoff_ms, continuation);
        let due_at = Instant::now() + Duration::from_millis(delay_ms);

        self.cancel_retry_timer(&issue_id);
        self.state.retry_attempts.insert(
            issue_id.clone(),
            RetryEntry {
                issue_id: issue_id.clone(),
                identifier,
                attempt,
                due_at,
                error,
            },
        );
        self.state.claimed.insert(issue_id.clone());

        let cmd_tx = self.cmd_tx.clone();
        let issue_for_timer = issue_id.clone();
        let task = tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(delay_ms)).await;
            let _ = cmd_tx
                .send(OrchestratorCommand::RetryFire {
                    issue_id: issue_for_timer,
                })
                .await;
        });
        self.retry_tasks.insert(issue_id, task);
    }

    fn cancel_retry_timer(&mut self, issue_id: &str) {
        if let Some(handle) = self.retry_tasks.remove(issue_id) {
            handle.abort();
        }
    }

    async fn handle_retry_fire(&mut self, issue_id: &str) {
        let retry_entry = match self.state.retry_attempts.remove(issue_id) {
            Some(e) => e,
            None => return,
        };

        let candidates = match self.tracker.fetch_candidate_issues().await {
            Ok(v) => v,
            Err(_) => {
                self.schedule_retry(
                    issue_id.to_string(),
                    retry_entry.identifier,
                    retry_entry.attempt + 1,
                    Some("retry poll failed".into()),
                    false,
                );
                return;
            }
        };

        let issue = match candidates.into_iter().find(|c| c.id == issue_id) {
            Some(i) => i,
            None => {
                self.state.claimed.remove(issue_id);
                return;
            }
        };

        let verdict = dispatch_eligibility(&issue, &self.cfg, &self.state);
        match verdict.reason {
            EligibilityVerdict::Ok => {
                self.dispatch(issue, Some(retry_entry.attempt));
            }
            EligibilityVerdict::GlobalSlotsExhausted
            | EligibilityVerdict::PerStateSlotsExhausted => {
                self.schedule_retry(
                    issue_id.to_string(),
                    issue.identifier,
                    retry_entry.attempt + 1,
                    Some("no available orchestrator slots".into()),
                    false,
                );
            }
            EligibilityVerdict::NotInActiveStates
            | EligibilityVerdict::InTerminalStates
            | EligibilityVerdict::BlockedByOpenBlocker
            | EligibilityVerdict::AlreadyRunning
            | EligibilityVerdict::AlreadyClaimed
            | EligibilityVerdict::MissingFields => {
                self.state.claimed.remove(issue_id);
            }
        }
    }

    fn apply_codex_update(&mut self, issue_id: &str, event: RuntimeEvent) {
        let entry = match self.state.running.get_mut(issue_id) {
            Some(e) => e,
            None => return,
        };
        entry.session.last_codex_event = Some(event.event.clone());
        entry.session.last_codex_message = event.message.clone();
        entry.session.last_codex_timestamp_monotonic = Some(Instant::now());
        entry.session.last_codex_timestamp = Some(event.timestamp);
        entry.session.session_id = event
            .session_id
            .clone()
            .or(entry.session.session_id.clone());
        entry.session.thread_id = event.thread_id.clone().or(entry.session.thread_id.clone());
        entry.session.turn_id = event.turn_id.clone().or(entry.session.turn_id.clone());
        if event.event == "session_started" {
            entry.session.turn_count = entry.session.turn_count.saturating_add(1);
        }
        if let Some(absolute) = event.thread_total_usage {
            // Track deltas against last reported absolute totals.
            let in_delta = absolute
                .input_tokens
                .saturating_sub(entry.session.last_reported_input_tokens);
            let out_delta = absolute
                .output_tokens
                .saturating_sub(entry.session.last_reported_output_tokens);
            let total_delta = absolute
                .total_tokens
                .saturating_sub(entry.session.last_reported_total_tokens);
            entry.session.codex_input_tokens = absolute.input_tokens;
            entry.session.codex_output_tokens = absolute.output_tokens;
            entry.session.codex_total_tokens = absolute.total_tokens;
            entry.session.last_reported_input_tokens = absolute.input_tokens;
            entry.session.last_reported_output_tokens = absolute.output_tokens;
            entry.session.last_reported_total_tokens = absolute.total_tokens;
            self.state.codex_totals.input_tokens = self
                .state
                .codex_totals
                .input_tokens
                .saturating_add(in_delta);
            self.state.codex_totals.output_tokens = self
                .state
                .codex_totals
                .output_tokens
                .saturating_add(out_delta);
            self.state.codex_totals.total_tokens = self
                .state
                .codex_totals
                .total_tokens
                .saturating_add(total_delta);
        }
    }

    fn update_runtime_total(&mut self, entry: &RunningEntry) {
        let elapsed = entry.started_monotonic.elapsed().as_secs_f64();
        self.state.codex_totals.seconds_running += elapsed;
    }

    fn snapshot(&self) -> Snapshot {
        let now = Instant::now();
        let now_utc = OffsetDateTime::now_utc();
        let running = self
            .state
            .running
            .iter()
            .map(|(id, e)| SnapshotRunningRow {
                issue_id: id.clone(),
                identifier: e.identifier.clone(),
                state: e.issue.state.clone(),
                session_id: e.session.session_id.clone(),
                turn_count: e.session.turn_count,
                last_event: e.session.last_codex_event.clone(),
                last_message: e.session.last_codex_message.clone(),
                started_at: e.started_at,
                last_event_at: e.session.last_codex_timestamp,
                input_tokens: e.session.codex_input_tokens,
                output_tokens: e.session.codex_output_tokens,
                total_tokens: e.session.codex_total_tokens,
            })
            .collect();
        let retrying = self
            .state
            .retry_attempts
            .values()
            .map(|r| SnapshotRetryRow {
                issue_id: r.issue_id.clone(),
                identifier: r.identifier.clone(),
                attempt: r.attempt,
                due_in_ms: r
                    .due_at
                    .saturating_duration_since(now)
                    .as_millis()
                    .min(i64::MAX as u128) as i64,
                error: r.error.clone(),
            })
            .collect();
        let mut totals = self.state.codex_totals.clone();
        for entry in self.state.running.values() {
            totals.seconds_running += entry.started_monotonic.elapsed().as_secs_f64();
        }
        Snapshot {
            generated_at: now_utc,
            running,
            retrying,
            codex_totals: totals,
        }
    }

    async fn shutdown(&mut self) {
        if let Some(h) = self.scheduled_tick.take() {
            h.abort();
        }
        for (_, h) in self.worker_tasks.drain() {
            h.abort();
        }
        for (_, h) in self.retry_tasks.drain() {
            h.abort();
        }
        for reply in self.pending_refresh_replies.drain(..) {
            let _ = reply.send(());
        }
    }
}
