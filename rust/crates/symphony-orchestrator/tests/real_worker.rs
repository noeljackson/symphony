//! Integration tests for [`RealWorker`] using a tiny bash-based fake codex
//! that speaks just enough JSON-RPC to drive a single turn to completion.
//!
//! The fake codex echoes line-delimited JSON canned responses keyed off the
//! incoming request id, so we can verify the worker performs the full
//! workspace + hook + handshake + turn + after_run dance without depending
//! on the real `codex app-server` binary.

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use serde_yaml::Mapping;
use symphony_codex::events::RuntimeEvent;
use symphony_core::config::{
    AgentConfig, CodexConfig, HooksConfig, PollingConfig, ServerConfig, ServiceConfig,
    TrackerConfig, TrackerKind, WorkspaceConfig,
};
use symphony_core::issue::Issue;
use symphony_core::prompt::PromptBuilder;
use symphony_orchestrator::{RealWorker, WorkerOutcome, WorkerRunner};
use symphony_tracker::memory::MemoryTracker;
use symphony_workspace::WorkspaceManager;
use tempfile::TempDir;
use tokio::sync::mpsc;

/// A bash one-liner that listens on stdin and replies on stdout with the
/// minimum sequence required to make a turn complete:
///
/// 1. read 1 line (`initialize`) → reply `{"id":1,"result":{}}`
/// 2. read 1 line (`initialized` notification, no reply needed)
/// 3. read 1 line (`thread/start`) → reply `{"id":2,"result":{"thread":{"id":"thr"}}}`
/// 4. read 1 line (`turn/start`) → reply `{"id":3,"result":{"turn":{"id":"trn"}}}`
/// 5. emit `{"method":"turn/completed","params":{}}`
/// 6. exit cleanly
const FAKE_CODEX: &str = r#"
read -r line
echo '{"id":1,"result":{}}'
read -r line
read -r line
echo '{"id":2,"result":{"thread":{"id":"thr"}}}'
read -r line
echo '{"id":3,"result":{"turn":{"id":"trn"}}}'
echo '{"method":"turn/completed","params":{}}'
"#;

fn cfg(workspace_root: PathBuf, command: String, before_run: Option<String>) -> Arc<ServiceConfig> {
    Arc::new(ServiceConfig {
        tracker: TrackerConfig {
            kind: TrackerKind::Linear,
            endpoint: "https://example".into(),
            api_key: Some("k".into()),
            project_slug: Some("demo".into()),
            active_states: vec!["Todo".into(), "In Progress".into()],
            terminal_states: vec!["Done".into()],
        },
        polling: PollingConfig {
            interval_ms: 30_000,
        },
        workspace: WorkspaceConfig {
            root: workspace_root,
        },
        hooks: HooksConfig {
            timeout_ms: 5_000,
            before_run,
            ..Default::default()
        },
        agent: AgentConfig {
            max_concurrent_agents: 1,
            max_turns: 1,
            max_retry_backoff_ms: 300_000,
            max_concurrent_agents_by_state: std::collections::BTreeMap::new(),
        },
        codex: CodexConfig {
            command,
            approval_policy: Some(serde_yaml::Value::String("never".into())),
            thread_sandbox: Some(serde_yaml::Value::String("danger-full-access".into())),
            turn_sandbox_policy: None,
            turn_timeout_ms: 5_000,
            read_timeout_ms: 1_000,
            stall_timeout_ms: 60_000,
        },
        server: ServerConfig { port: None },
        raw: Mapping::new(),
        workflow_path: PathBuf::from("/tmp/WORKFLOW.md"),
    })
}

fn issue(state: &str) -> Issue {
    Issue {
        id: "id-1".into(),
        identifier: "MT-1".into(),
        title: "do thing".into(),
        description: None,
        priority: Some(2),
        state: state.into(),
        branch_name: None,
        url: None,
        labels: vec![],
        blocked_by: vec![],
        created_at: None,
        updated_at: None,
    }
}

#[tokio::test]
async fn real_worker_runs_turn_to_success_when_state_goes_terminal() {
    let root = TempDir::new().unwrap();
    let cfg = cfg(root.path().to_path_buf(), FAKE_CODEX.into(), None);

    // Tracker reports the issue going terminal after one turn so the worker
    // exits the continuation loop without doing another turn.
    let tracker = Arc::new(MemoryTracker::with_issues(vec![issue("Done")]));
    let workspace_mgr = Arc::new(WorkspaceManager::new(
        cfg.workspace.root.clone(),
        cfg.hooks.timeout_ms,
        None,
        None,
    ));
    let prompt = Arc::new(PromptBuilder::new("hello {{ issue.identifier }}"));
    let runner = RealWorker::new(cfg.clone(), workspace_mgr.clone(), tracker.clone(), prompt);

    let (events_tx, mut events_rx) = mpsc::channel::<RuntimeEvent>(64);
    let outcome = runner.run(issue("Todo"), None, events_tx).await;

    assert_eq!(outcome, WorkerOutcome::Success);
    assert!(
        root.path().join("MT-1").is_dir(),
        "workspace must be reused"
    );

    let mut events = Vec::new();
    while let Ok(ev) = tokio::time::timeout(Duration::from_millis(50), events_rx.recv()).await {
        match ev {
            Some(e) => events.push(e.event),
            None => break,
        }
    }
    assert!(events.iter().any(|e| e == "session_started"));
    assert!(events.iter().any(|e| e == "turn_completed"));
}

#[tokio::test]
async fn real_worker_aborts_when_before_run_fails() {
    let root = TempDir::new().unwrap();
    let cfg = cfg(
        root.path().to_path_buf(),
        FAKE_CODEX.into(),
        Some("exit 7".into()),
    );
    let tracker = Arc::new(MemoryTracker::with_issues(vec![issue("Todo")]));
    let workspace_mgr = Arc::new(WorkspaceManager::new(
        cfg.workspace.root.clone(),
        cfg.hooks.timeout_ms,
        None,
        None,
    ));
    let prompt = Arc::new(PromptBuilder::new("hello"));
    let runner = RealWorker::new(cfg.clone(), workspace_mgr, tracker, prompt);

    let (events_tx, _events_rx) = mpsc::channel::<RuntimeEvent>(8);
    let outcome = runner.run(issue("Todo"), None, events_tx).await;
    match outcome {
        WorkerOutcome::Failure { error } => assert!(error.starts_with("before_run:")),
        other => panic!("unexpected outcome: {other:?}"),
    }
}

#[tokio::test]
async fn real_worker_fails_when_codex_command_missing() {
    let root = TempDir::new().unwrap();
    let cfg = cfg(
        root.path().to_path_buf(),
        "definitely-not-on-PATH-symphony-test".into(),
        None,
    );
    let tracker = Arc::new(MemoryTracker::with_issues(vec![issue("Todo")]));
    let workspace_mgr = Arc::new(WorkspaceManager::new(
        cfg.workspace.root.clone(),
        cfg.hooks.timeout_ms,
        None,
        None,
    ));
    let prompt = Arc::new(PromptBuilder::new("hello"));
    let runner = RealWorker::new(cfg, workspace_mgr, tracker, prompt);

    let (events_tx, _events_rx) = mpsc::channel::<RuntimeEvent>(8);
    let outcome = runner.run(issue("Todo"), None, events_tx).await;
    match outcome {
        WorkerOutcome::Failure { error } => {
            // Either codex_not_found (spawn failure) or startup_failed (port exit
            // because bash exited 127). Both are acceptable failure surfaces.
            assert!(
                error.starts_with("codex_not_found:") || error.starts_with("startup_failed:"),
                "unexpected error: {error}"
            );
        }
        other => panic!("unexpected outcome: {other:?}"),
    }
}
