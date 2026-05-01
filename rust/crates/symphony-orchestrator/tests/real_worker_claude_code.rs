//! Integration test for [`RealWorker`] running against the `claude_code`
//! backend. Uses a tiny bash-driven fake `claude` CLI that emits the
//! stream-json messages the client needs (system/init then result/success)
//! to drive a single turn end-to-end without depending on the real binary.

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use serde_yaml::Mapping;
use symphony_codex::events::RuntimeEvent;
use symphony_core::config::{
    AgentBackend, AgentConfig, ClaudeCodeConfig, CodexConfig, HooksConfig, PollingConfig,
    ServerConfig, ServiceConfig, TrackerConfig, TrackerKind, WorkspaceConfig,
};
use symphony_core::issue::Issue;
use symphony_core::prompt::PromptBuilder;
use symphony_orchestrator::{RealWorker, WorkerOutcome, WorkerRunner};
use symphony_tracker::memory::MemoryTracker;
use symphony_workspace::WorkspaceManager;
use tempfile::TempDir;
use tokio::sync::mpsc;

/// Bash fake-claude:
///
/// 1. emit `system`/`init` immediately so start_session resolves
/// 2. read one user message from stdin (the rendered prompt)
/// 3. emit a `result`/`success` so the turn completes
/// 4. exit cleanly
const FAKE_CLAUDE: &str = r#"
echo '{"type":"system","subtype":"init","cwd":"'"$PWD"'","session_id":"sess-fake-1","tools":[],"mcp_servers":[],"model":"fake","permissionMode":"bypassPermissions","apiKeySource":"env"}'
read -r line
echo '{"type":"result","subtype":"success","session_id":"sess-fake-1","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}'
"#;

fn cfg(workspace_root: PathBuf, command: String) -> Arc<ServiceConfig> {
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
            ..Default::default()
        },
        agent: AgentConfig {
            backend: AgentBackend::ClaudeCode,
            max_concurrent_agents: 1,
            max_turns: 1,
            max_retry_backoff_ms: 300_000,
            max_concurrent_agents_by_state: std::collections::BTreeMap::new(),
            daily_budget_usd: None,
        },
        codex: CodexConfig {
            command: "true".into(),
            approval_policy: None,
            thread_sandbox: None,
            turn_sandbox_policy: None,
            turn_timeout_ms: 5_000,
            read_timeout_ms: 1_000,
            stall_timeout_ms: 60_000,
        },
        claude_code: ClaudeCodeConfig {
            command,
            permission_mode: Some("bypassPermissions".into()),
            allowed_tools: Vec::new(),
            disallowed_tools: Vec::new(),
            model: None,
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
async fn real_worker_runs_claude_code_turn_to_success() {
    let root = TempDir::new().unwrap();
    let cfg = cfg(root.path().to_path_buf(), FAKE_CLAUDE.into());

    // Tracker reports the issue going terminal after one turn so the worker
    // exits the continuation loop.
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
        "workspace must be created"
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
async fn real_worker_claude_code_fails_when_command_missing() {
    let root = TempDir::new().unwrap();
    let cfg = cfg(
        root.path().to_path_buf(),
        "definitely-not-on-PATH-claude-test".into(),
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
            assert!(
                error.starts_with("agent_runner_not_found:")
                    || error.starts_with("startup_failed:"),
                "unexpected error: {error}"
            );
        }
        other => panic!("unexpected outcome: {other:?}"),
    }
}
