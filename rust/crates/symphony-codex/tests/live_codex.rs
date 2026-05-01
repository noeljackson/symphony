//! SPEC §17.8 Real Integration Profile — opt-in smoke test against a real
//! `codex app-server` binary on the host PATH. Skipped by default; enable with
//! `cargo test -p symphony-codex --test live_codex -- --ignored`.
//!
//! The test only drives the JSON-RPC handshake (`initialize` → `initialized`
//! → `thread/start`) and immediately stops the session, so it does not start
//! a turn and therefore does NOT call the LLM or incur API costs. The full
//! turn smoke is an additional opt-in below, gated by
//! `SYMPHONY_E2E_REAL_CODEX_FULL=1`.

use std::path::PathBuf;
use std::process::Stdio;
use std::time::Duration;

use symphony_codex::channel::ChildChannel;
use symphony_codex::client::{CodexClient, CodexLaunch, SessionPolicies};
use symphony_codex::events::RuntimeEvent;
use tempfile::TempDir;
use tokio::process::Command;
use tokio::sync::mpsc;

/// Expected location of the real codex CLI. We allow the test runner to
/// override via `CODEX_BIN` so dev environments that ship codex under a
/// different name can still run the smoke test.
fn codex_command() -> Option<String> {
    if let Ok(custom) = std::env::var("CODEX_BIN") {
        if !custom.trim().is_empty() {
            return Some(custom);
        }
    }
    if which("codex") {
        Some("codex app-server".to_string())
    } else {
        None
    }
}

fn which(name: &str) -> bool {
    std::env::var_os("PATH")
        .into_iter()
        .flat_map(|p| std::env::split_paths(&p).collect::<Vec<_>>())
        .any(|dir| dir.join(name).is_file() || dir.join(name).is_symlink())
}

#[tokio::test]
#[ignore = "real codex; run with --ignored or set SYMPHONY_E2E_REAL_CODEX=1"]
async fn live_codex_handshake_smoke() {
    let cmd = match codex_command() {
        Some(c) => c,
        None => panic!(
            "real-codex smoke requires the `codex` binary on PATH (or set CODEX_BIN). \
             This test was explicitly opted in via --ignored."
        ),
    };

    let workspace = TempDir::new().unwrap();
    let workspace_path: PathBuf = workspace.path().to_path_buf();
    let cwd_str = workspace_path.to_string_lossy().to_string();

    let mut command = Command::new("bash");
    command
        .arg("-lc")
        .arg(&cmd)
        .current_dir(&workspace_path)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .kill_on_drop(true);
    let child = command
        .spawn()
        .unwrap_or_else(|e| panic!("failed to spawn `{cmd}`: {e}"));
    let channel = ChildChannel::new(child).expect("ChildChannel");

    let (events_tx, events_rx) = mpsc::channel::<RuntimeEvent>(16);
    let launch = CodexLaunch {
        workspace: workspace_path.clone(),
        policies: SessionPolicies::default(),
        read_timeout: Duration::from_secs(15),
        turn_timeout: Duration::from_secs(15),
    };
    let mut client = CodexClient::new(channel, events_tx, launch);

    let thread_id = client
        .start_session(&cwd_str)
        .await
        .expect("real codex handshake should complete cleanly");
    assert!(!thread_id.is_empty(), "thread id should be non-empty");

    client.stop_session().await;
    drop(events_rx);
}

/// Full turn smoke: spawns codex, completes a single turn against the live
/// model, and asserts we observe `session_started` + `turn_completed` events.
/// This DOES make an LLM call and incurs API costs, so it's behind a second
/// opt-in env var on top of `--ignored`.
#[tokio::test]
#[ignore = "real codex turn; run with --ignored AND SYMPHONY_E2E_REAL_CODEX_FULL=1"]
async fn live_codex_turn_smoke() {
    if std::env::var("SYMPHONY_E2E_REAL_CODEX_FULL")
        .ok()
        .as_deref()
        != Some("1")
    {
        panic!(
            "set SYMPHONY_E2E_REAL_CODEX_FULL=1 to opt into the live turn smoke; \
             this test makes a real LLM call against codex"
        );
    }
    let cmd = match codex_command() {
        Some(c) => c,
        None => panic!("real-codex turn smoke requires `codex` on PATH (or CODEX_BIN)"),
    };

    let workspace = TempDir::new().unwrap();
    let workspace_path: PathBuf = workspace.path().to_path_buf();
    let cwd_str = workspace_path.to_string_lossy().to_string();

    let mut command = Command::new("bash");
    command
        .arg("-lc")
        .arg(&cmd)
        .current_dir(&workspace_path)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .kill_on_drop(true);
    let child = command.spawn().expect("spawn codex");
    let channel = ChildChannel::new(child).expect("ChildChannel");

    let (events_tx, mut events_rx) = mpsc::channel::<RuntimeEvent>(64);
    let launch = CodexLaunch {
        workspace: workspace_path.clone(),
        policies: SessionPolicies::default(),
        read_timeout: Duration::from_secs(30),
        turn_timeout: Duration::from_secs(120),
    };
    let mut client = CodexClient::new(channel, events_tx, launch);

    let _thread_id = client.start_session(&cwd_str).await.expect("start_session");
    let req = symphony_codex::client::TurnRequest {
        prompt: "Reply with the single word OK.".into(),
        title: "live-smoke".into(),
    };
    let summary = client
        .run_turn(req, &cwd_str)
        .await
        .expect("turn should complete");
    assert!(!summary.session_id.is_empty());

    let mut events = Vec::new();
    while let Ok(Some(ev)) = tokio::time::timeout(Duration::from_millis(50), events_rx.recv()).await
    {
        events.push(ev.event);
    }
    assert!(events.contains(&"session_started".to_string()));
    assert!(events.contains(&"turn_completed".to_string()));

    client.stop_session().await;
}
