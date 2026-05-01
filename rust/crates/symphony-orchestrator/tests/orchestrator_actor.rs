//! End-to-end orchestrator tests using a scripted worker so we exercise the
//! actor against deterministic outcomes without needing real Codex /
//! workspace.

use std::path::PathBuf;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use async_trait::async_trait;
use serde_yaml::Mapping;
use symphony_codex::events::RuntimeEvent;
use symphony_core::config::{
    AgentConfig, CodexConfig, HooksConfig, PollingConfig, ServerConfig, ServiceConfig,
    TrackerConfig, TrackerKind, WorkspaceConfig,
};
use symphony_core::issue::Issue;
use symphony_orchestrator::{
    Orchestrator, OrchestratorCommand, OrchestratorHandle, WorkerOutcome, WorkerRunner,
};
use symphony_tracker::memory::MemoryTracker;
use symphony_tracker::Tracker;
use tokio::sync::{mpsc, Notify};
use tokio::time::Instant;

#[derive(Default)]
struct ScriptedRunner {
    outcomes: Mutex<Vec<WorkerOutcome>>,
    started: Mutex<Vec<(String, Option<u32>)>>,
    started_notify: Notify,
    finish_gate: Mutex<Vec<Arc<Notify>>>,
}

impl ScriptedRunner {
    fn new(outcomes: Vec<WorkerOutcome>) -> Arc<Self> {
        Arc::new(Self {
            outcomes: Mutex::new(outcomes),
            started: Mutex::new(Vec::new()),
            started_notify: Notify::new(),
            finish_gate: Mutex::new(Vec::new()),
        })
    }

    fn dispatched(&self) -> Vec<(String, Option<u32>)> {
        self.started.lock().unwrap().clone()
    }

    /// Block subsequent worker tasks until `release` is called on the returned
    /// gate. Used to keep workers "running" while we tick reconciliation.
    fn arm_gate(&self) -> Arc<Notify> {
        let gate = Arc::new(Notify::new());
        self.finish_gate.lock().unwrap().push(gate.clone());
        gate
    }
}

#[async_trait]
impl WorkerRunner for ScriptedRunner {
    async fn run(
        &self,
        issue: Issue,
        attempt: Option<u32>,
        _events: mpsc::Sender<RuntimeEvent>,
    ) -> WorkerOutcome {
        self.started
            .lock()
            .unwrap()
            .push((issue.identifier.clone(), attempt));
        self.started_notify.notify_waiters();

        let gate = self.finish_gate.lock().unwrap().pop();
        if let Some(g) = gate {
            g.notified().await;
        }

        let mut outs = self.outcomes.lock().unwrap();
        if outs.is_empty() {
            WorkerOutcome::Success
        } else {
            outs.remove(0)
        }
    }
}

fn make_config(max_concurrent: usize) -> Arc<ServiceConfig> {
    Arc::new(ServiceConfig {
        tracker: TrackerConfig {
            kind: TrackerKind::Linear,
            endpoint: "https://example".into(),
            api_key: Some("k".into()),
            project_slug: Some("demo".into()),
            active_states: vec!["Todo".into(), "In Progress".into()],
            terminal_states: vec!["Done".into(), "Cancelled".into()],
        },
        polling: PollingConfig {
            interval_ms: 30_000,
        },
        workspace: WorkspaceConfig {
            root: PathBuf::from("/tmp/sym-test"),
        },
        hooks: HooksConfig {
            timeout_ms: 60_000,
            ..Default::default()
        },
        agent: AgentConfig {
            backend: symphony_core::config::AgentBackend::Codex,
            max_concurrent_agents: max_concurrent,
            max_turns: 20,
            max_retry_backoff_ms: 300_000,
            max_concurrent_agents_by_state: std::collections::BTreeMap::new(),
        },
        codex: CodexConfig {
            command: "codex".into(),
            approval_policy: None,
            thread_sandbox: None,
            turn_sandbox_policy: None,
            turn_timeout_ms: 3_600_000,
            read_timeout_ms: 5_000,
            stall_timeout_ms: 300_000,
        },
        claude_code: symphony_core::config::ClaudeCodeConfig {
            command: "true".into(),
            permission_mode: None,
            allowed_tools: Vec::new(),
            disallowed_tools: Vec::new(),
            model: None,
            turn_timeout_ms: 3_600_000,
            read_timeout_ms: 5_000,
            stall_timeout_ms: 300_000,
        },
        server: ServerConfig { port: None },
        raw: Mapping::new(),
        workflow_path: PathBuf::from("/tmp/WORKFLOW.md"),
    })
}

fn issue(id: &str, identifier: &str, state: &str) -> Issue {
    Issue {
        id: id.into(),
        identifier: identifier.into(),
        title: "t".into(),
        description: None,
        priority: Some(1),
        state: state.into(),
        branch_name: None,
        url: None,
        labels: vec![],
        blocked_by: vec![],
        created_at: Some(time::macros::datetime!(2026-01-01 00:00 UTC)),
        updated_at: None,
    }
}

async fn spawn_actor(
    cfg: Arc<ServiceConfig>,
    tracker: Arc<dyn Tracker>,
    runner: Arc<dyn WorkerRunner>,
) -> (OrchestratorHandle, tokio::task::JoinHandle<()>) {
    let (actor, handle) = Orchestrator::new(cfg, tracker, runner);
    let join = tokio::spawn(async move {
        let _ = actor.run().await;
    });
    (handle, join)
}

/// Wait for `runner` to start `count` workers, with a generous timeout.
async fn wait_for_dispatches(runner: &ScriptedRunner, count: usize) {
    let deadline = Instant::now() + Duration::from_secs(2);
    while runner.dispatched().len() < count {
        if Instant::now() >= deadline {
            panic!(
                "expected {count} dispatches, got {}",
                runner.dispatched().len()
            );
        }
        tokio::time::sleep(Duration::from_millis(5)).await;
    }
}

#[tokio::test]
async fn dispatches_eligible_issues_in_priority_order() {
    let cfg = make_config(10);
    let mut high = issue("a", "MT-1", "Todo");
    high.priority = Some(1);
    let mut low = issue("b", "MT-2", "Todo");
    low.priority = Some(3);
    let tracker = Arc::new(MemoryTracker::with_issues(vec![low, high]));
    let runner = ScriptedRunner::new(vec![]);
    let _gate1 = runner.arm_gate();
    let _gate2 = runner.arm_gate();

    let (handle, _join) = spawn_actor(cfg, tracker, runner.clone()).await;
    handle.tick().await;
    wait_for_dispatches(&runner, 2).await;
    let dispatched: Vec<String> = runner.dispatched().into_iter().map(|(id, _)| id).collect();
    assert_eq!(dispatched, vec!["MT-1", "MT-2"]);
    handle.shutdown().await;
}

#[tokio::test]
async fn global_concurrency_cap_limits_dispatch() {
    let cfg = make_config(1);
    let issues: Vec<Issue> = (0..3)
        .map(|i| issue(&format!("id-{i}"), &format!("MT-{i}"), "Todo"))
        .collect();
    let tracker = Arc::new(MemoryTracker::with_issues(issues));
    let runner = ScriptedRunner::new(vec![]);
    let _g = runner.arm_gate();

    let (handle, _join) = spawn_actor(cfg, tracker, runner.clone()).await;
    handle.tick().await;
    wait_for_dispatches(&runner, 1).await;
    // Give the actor a beat to attempt more dispatches.
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert_eq!(runner.dispatched().len(), 1);
    handle.shutdown().await;
}

#[tokio::test]
async fn worker_exit_with_failure_schedules_exponential_retry() {
    let cfg = make_config(10);
    let tracker = Arc::new(MemoryTracker::with_issues(vec![issue("a", "MT-1", "Todo")]));
    let runner = ScriptedRunner::new(vec![WorkerOutcome::Failure {
        error: "boom".into(),
    }]);

    let (handle, _join) = spawn_actor(cfg, tracker, runner.clone()).await;
    handle.tick().await;
    wait_for_dispatches(&runner, 1).await;

    // Wait for the worker to finish and the retry to be queued.
    let deadline = Instant::now() + Duration::from_secs(2);
    loop {
        let snap = handle.snapshot().await.unwrap();
        if !snap.retrying.is_empty() {
            assert_eq!(snap.retrying[0].identifier, "MT-1");
            assert_eq!(snap.retrying[0].attempt, 1);
            // Failure backoff should be ~10s (allow some skew).
            assert!(snap.retrying[0].due_in_ms > 5_000);
            break;
        }
        if Instant::now() >= deadline {
            panic!("retry was not scheduled");
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    handle.shutdown().await;
}

#[tokio::test]
async fn successful_worker_exit_schedules_short_continuation_retry() {
    let cfg = make_config(10);
    let tracker = Arc::new(MemoryTracker::with_issues(vec![issue("a", "MT-1", "Todo")]));
    let runner = ScriptedRunner::new(vec![WorkerOutcome::Success]);

    let (handle, _join) = spawn_actor(cfg, tracker, runner.clone()).await;
    handle.tick().await;
    wait_for_dispatches(&runner, 1).await;

    let deadline = Instant::now() + Duration::from_secs(2);
    loop {
        let snap = handle.snapshot().await.unwrap();
        if !snap.retrying.is_empty() {
            // Continuation delay is ~1s.
            assert!(snap.retrying[0].due_in_ms <= 1_500);
            assert_eq!(snap.retrying[0].attempt, 1);
            break;
        }
        if Instant::now() >= deadline {
            panic!("continuation retry was not scheduled");
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    handle.shutdown().await;
}

#[tokio::test]
async fn reconcile_terminates_running_when_state_goes_terminal() {
    let cfg = make_config(10);
    let tracker = Arc::new(MemoryTracker::with_issues(vec![issue("a", "MT-1", "Todo")]));
    let runner = ScriptedRunner::new(vec![]);
    let _gate = runner.arm_gate();

    let (handle, _join) = spawn_actor(cfg, tracker.clone(), runner.clone()).await;
    handle.tick().await;
    wait_for_dispatches(&runner, 1).await;

    // Flip tracker state to terminal and tick.
    tracker.replace(vec![issue("a", "MT-1", "Done")]);
    handle.tick().await;

    let deadline = Instant::now() + Duration::from_secs(2);
    loop {
        let snap = handle.snapshot().await.unwrap();
        if snap.running.is_empty() {
            break;
        }
        if Instant::now() >= deadline {
            panic!("reconcile did not stop the running worker");
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    handle.shutdown().await;
}

#[tokio::test]
async fn refresh_now_returns_when_tick_finishes() {
    let cfg = make_config(10);
    let tracker = Arc::new(MemoryTracker::with_issues(vec![]));
    let runner = ScriptedRunner::new(vec![]);
    let (handle, _join) = spawn_actor(cfg, tracker, runner).await;
    let acked = handle.refresh_now().await;
    assert!(acked);
    handle.shutdown().await;
}

#[tokio::test]
async fn snapshot_reports_running_and_agent_totals() {
    let cfg = make_config(10);
    let tracker = Arc::new(MemoryTracker::with_issues(vec![issue("a", "MT-1", "Todo")]));
    let runner = ScriptedRunner::new(vec![]);
    let _gate = runner.arm_gate();
    let (handle, _join) = spawn_actor(cfg, tracker, runner.clone()).await;
    handle.tick().await;
    wait_for_dispatches(&runner, 1).await;

    // Push a token usage update through the orchestrator command channel.
    let mut event = RuntimeEvent::new("notification");
    event.thread_total_usage = Some(symphony_codex::events::TokenUsage {
        input_tokens: 100,
        output_tokens: 40,
        total_tokens: 140,
    });
    handle
        .raw_sender()
        .send(OrchestratorCommand::AgentUpdate {
            issue_id: "a".into(),
            event: Box::new(event),
        })
        .await
        .unwrap();
    // Allow the actor a moment to drain that command.
    tokio::time::sleep(Duration::from_millis(50)).await;
    let snap = handle.snapshot().await.unwrap();
    assert_eq!(snap.running.len(), 1);
    assert_eq!(snap.running[0].identifier, "MT-1");
    assert_eq!(snap.agent_totals.total_tokens, 140);
    handle.shutdown().await;
}
