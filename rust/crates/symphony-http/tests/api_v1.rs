//! End-to-end HTTP tests: bind the axum server to an ephemeral port, talk to
//! it via reqwest, and assert the SPEC §13.7.2 shapes.

use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::Arc;

use async_trait::async_trait;
use serde_json::Value;
use serde_yaml::Mapping;
use symphony_codex::events::RuntimeEvent;
use symphony_core::config::{
    AgentConfig, CodexConfig, HooksConfig, PollingConfig, ServerConfig, ServiceConfig,
    TrackerConfig, TrackerKind, WorkspaceConfig,
};
use symphony_core::Issue;
use symphony_http::serve;
use symphony_orchestrator::{Orchestrator, WorkerOutcome, WorkerRunner};
use symphony_tracker::memory::MemoryTracker;
use tokio::sync::{mpsc, Notify};

#[derive(Default)]
struct StallRunner {
    gate: Notify,
}

#[async_trait]
impl WorkerRunner for StallRunner {
    async fn run(
        &self,
        _issue: Issue,
        _attempt: Option<u32>,
        _events: mpsc::Sender<RuntimeEvent>,
    ) -> WorkerOutcome {
        self.gate.notified().await;
        WorkerOutcome::Success
    }
}

fn cfg() -> Arc<ServiceConfig> {
    Arc::new(ServiceConfig {
        tracker: TrackerConfig {
            kind: TrackerKind::Linear,
            endpoint: "https://example".into(),
            api_key: Some("k".into()),
            project_slug: Some("demo".into()),
            active_states: vec!["Todo".into(), "In Progress".into()],
            terminal_states: vec!["Done".into()],
        },
        polling: PollingConfig { interval_ms: 30_000 },
        workspace: WorkspaceConfig {
            root: PathBuf::from("/tmp/sym-http"),
        },
        hooks: HooksConfig {
            timeout_ms: 60_000,
            ..Default::default()
        },
        agent: AgentConfig {
            max_concurrent_agents: 4,
            max_turns: 1,
            max_retry_backoff_ms: 300_000,
            max_concurrent_agents_by_state: std::collections::BTreeMap::new(),
        },
        codex: CodexConfig {
            command: "true".into(),
            approval_policy: None,
            thread_sandbox: None,
            turn_sandbox_policy: None,
            turn_timeout_ms: 3_600_000,
            read_timeout_ms: 5_000,
            stall_timeout_ms: 300_000,
        },
        server: ServerConfig { port: Some(0) },
        raw: Mapping::new(),
        workflow_path: PathBuf::from("/tmp/WORKFLOW.md"),
    })
}

fn issue() -> Issue {
    Issue {
        id: "id-1".into(),
        identifier: "MT-1".into(),
        title: "do thing".into(),
        description: None,
        priority: Some(1),
        state: "Todo".into(),
        branch_name: None,
        url: None,
        labels: vec![],
        blocked_by: vec![],
        created_at: Some(time::macros::datetime!(2026-01-01 00:00 UTC)),
        updated_at: None,
    }
}

async fn boot() -> (SocketAddr, symphony_http::ServerHandle, symphony_orchestrator::OrchestratorHandle) {
    let cfg = cfg();
    let tracker = Arc::new(MemoryTracker::with_issues(vec![issue()]));
    let runner = Arc::new(StallRunner::default());
    let (actor, handle) = Orchestrator::new(cfg, tracker, runner);
    tokio::spawn(async move {
        let _ = actor.run().await;
    });

    let server = serve("127.0.0.1:0".parse().unwrap(), handle.clone())
        .await
        .unwrap();
    let addr = server.local_addr;
    (addr, server, handle)
}

#[tokio::test]
async fn state_endpoint_returns_running_and_totals_after_dispatch() {
    let (addr, server, handle) = boot().await;
    handle.tick().await;

    // Wait for the stall runner to be running.
    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(2);
    loop {
        if let Some(snap) = handle.snapshot().await {
            if !snap.running.is_empty() {
                break;
            }
        }
        if std::time::Instant::now() >= deadline {
            panic!("worker never became running");
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }

    let url = format!("http://{addr}/api/v1/state");
    let body: Value = reqwest::get(&url).await.unwrap().json().await.unwrap();
    assert_eq!(body["counts"]["running"], 1);
    assert_eq!(body["counts"]["retrying"], 0);
    assert_eq!(body["running"][0]["issue_identifier"], "MT-1");
    assert!(body["codex_totals"].is_object());

    server.shutdown().await;
}

#[tokio::test]
async fn issue_endpoint_returns_404_for_unknown_identifier() {
    let (addr, server, _handle) = boot().await;
    let url = format!("http://{addr}/api/v1/UNKNOWN");
    let resp = reqwest::get(&url).await.unwrap();
    assert_eq!(resp.status().as_u16(), 404);
    let body: Value = resp.json().await.unwrap();
    assert_eq!(body["error"]["code"], "issue_not_found");
    server.shutdown().await;
}

#[tokio::test]
async fn issue_endpoint_returns_running_for_known_identifier() {
    let (addr, server, handle) = boot().await;
    handle.tick().await;

    let deadline = std::time::Instant::now() + std::time::Duration::from_secs(2);
    loop {
        if let Some(snap) = handle.snapshot().await {
            if !snap.running.is_empty() {
                break;
            }
        }
        if std::time::Instant::now() >= deadline {
            panic!("worker never became running");
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }

    let url = format!("http://{addr}/api/v1/MT-1");
    let body: Value = reqwest::get(&url).await.unwrap().json().await.unwrap();
    assert_eq!(body["status"], "running");
    assert_eq!(body["issue_identifier"], "MT-1");

    server.shutdown().await;
}

#[tokio::test]
async fn refresh_endpoint_returns_202_with_queued_payload() {
    let (addr, server, _handle) = boot().await;
    let url = format!("http://{addr}/api/v1/refresh");
    let resp = reqwest::Client::new().post(&url).send().await.unwrap();
    assert_eq!(resp.status().as_u16(), 202);
    let body: Value = resp.json().await.unwrap();
    assert_eq!(body["queued"], true);
    assert_eq!(body["operations"][0], "poll");
    server.shutdown().await;
}

#[tokio::test]
async fn dashboard_root_serves_html() {
    let (addr, server, _handle) = boot().await;
    let url = format!("http://{addr}/");
    let resp = reqwest::get(&url).await.unwrap();
    assert!(resp.status().is_success());
    let ct = resp
        .headers()
        .get("content-type")
        .map(|v| v.to_str().unwrap_or(""))
        .unwrap_or("")
        .to_string();
    assert!(ct.contains("text/html"));
    let body = resp.text().await.unwrap();
    assert!(body.contains("Symphony"));
    server.shutdown().await;
}
