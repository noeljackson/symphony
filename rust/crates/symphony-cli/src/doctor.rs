//! `symphony doctor`: first-run diagnostic.
//!
//! Runs the §6.3 dispatch preflight plus environment checks and prints a
//! pass/fail checklist. Exit `0` on full green, `1` on any failure.

use std::io::Write;
use std::path::Path;
use std::process::Stdio;
use std::time::Duration;

use symphony_core::config::{AgentBackend, ServiceConfig, TrackerKind};
use symphony_core::workflow::WorkflowLoader;
use symphony_tracker::linear::{LinearClient, LinearConfig};
use symphony_tracker::Tracker;
use tokio::process::Command;

/// One row in the doctor's output.
#[derive(Debug, Clone)]
pub struct CheckResult {
    pub name: &'static str,
    pub ok: bool,
    pub detail: String,
}

/// Public entry point. Returns `Ok(())` if every check passed; `Err(_)` is
/// the count of failures. Prints rows to `out` as it goes.
pub async fn run<W: Write>(workflow_path: &Path, out: &mut W) -> std::io::Result<usize> {
    let mut failures = 0;

    let mut report = |row: CheckResult| {
        if !row.ok {
            failures += 1;
        }
        let mark = if row.ok { "✓" } else { "✗" };
        writeln!(out, "{mark} {} — {}", row.name, row.detail).ok();
    };

    // 1. Workflow file loads + parses.
    let definition = match WorkflowLoader::load(workflow_path) {
        Ok(d) => {
            report(CheckResult {
                name: "workflow file loadable",
                ok: true,
                detail: format!("parsed {}", d.path.display()),
            });
            d
        }
        Err(e) => {
            report(CheckResult {
                name: "workflow file loadable",
                ok: false,
                detail: format!("{e}"),
            });
            // Without the workflow there's nothing else to check.
            writeln!(out, "\n{} check(s) failed", failures).ok();
            return Ok(failures);
        }
    };

    // 2. Typed config + dispatch preflight.
    let cfg = match ServiceConfig::from_workflow(&definition) {
        Ok(c) => {
            report(CheckResult {
                name: "config schema valid",
                ok: true,
                detail: format!(
                    "tracker={:?} backend={}",
                    cfg_tracker_kind(&c),
                    c.agent.backend.as_str()
                ),
            });
            c
        }
        Err(e) => {
            report(CheckResult {
                name: "config schema valid",
                ok: false,
                detail: format!("{e}"),
            });
            writeln!(out, "\n{} check(s) failed", failures).ok();
            return Ok(failures);
        }
    };

    match cfg.validate_for_dispatch() {
        Ok(()) => report(CheckResult {
            name: "dispatch preflight",
            ok: true,
            detail: "all required fields present".into(),
        }),
        Err(e) => report(CheckResult {
            name: "dispatch preflight",
            ok: false,
            detail: format!("{e}"),
        }),
    }

    // 3. Workspace root writable.
    report(check_workspace_writable(&cfg).await);

    // 4. Hook scripts parse.
    for row in check_hooks(&cfg).await {
        report(row);
    }

    // 5. Agent backend prerequisite.
    report(check_agent_backend(&cfg).await);

    // 6. Tracker auth reachable (live network — best-effort, time-limited).
    report(check_tracker_reachable(&cfg).await);

    if failures == 0 {
        writeln!(out, "\nall checks passed").ok();
    } else {
        writeln!(out, "\n{} check(s) failed", failures).ok();
    }
    Ok(failures)
}

fn cfg_tracker_kind(cfg: &ServiceConfig) -> &str {
    match cfg.tracker.kind {
        TrackerKind::Linear => "linear",
        TrackerKind::Other(ref k) => k.as_str(),
    }
}

async fn check_workspace_writable(cfg: &ServiceConfig) -> CheckResult {
    let root = &cfg.workspace.root;
    let probe = root.join(".symphony-doctor-probe");
    match tokio::fs::create_dir_all(root).await {
        Ok(()) => {}
        Err(e) => {
            return CheckResult {
                name: "workspace root writable",
                ok: false,
                detail: format!("create_dir_all({}) failed: {e}", root.display()),
            };
        }
    }
    match tokio::fs::write(&probe, b"ok".as_slice()).await {
        Ok(()) => {
            let _ = tokio::fs::remove_file(&probe).await;
            CheckResult {
                name: "workspace root writable",
                ok: true,
                detail: format!("{}", root.display()),
            }
        }
        Err(e) => CheckResult {
            name: "workspace root writable",
            ok: false,
            detail: format!("write probe to {} failed: {e}", probe.display()),
        },
    }
}

async fn check_hooks(cfg: &ServiceConfig) -> Vec<CheckResult> {
    let hooks = [
        ("hooks.after_create", cfg.hooks.after_create.as_deref()),
        ("hooks.before_run", cfg.hooks.before_run.as_deref()),
        ("hooks.after_run", cfg.hooks.after_run.as_deref()),
        ("hooks.before_remove", cfg.hooks.before_remove.as_deref()),
    ];
    let mut out = Vec::new();
    for (name, script) in hooks {
        let Some(script) = script else { continue };
        if script.trim().is_empty() {
            continue;
        }
        out.push(parse_with_bash(name, script).await);
    }
    if out.is_empty() {
        out.push(CheckResult {
            name: "workspace hooks",
            ok: true,
            detail: "no hooks configured".into(),
        });
    }
    out
}

async fn parse_with_bash(name: &'static str, script: &str) -> CheckResult {
    // `bash -n` parses without executing. Fast and side-effect-free.
    let mut cmd = Command::new("bash");
    cmd.arg("-n")
        .arg("/dev/stdin")
        .stdin(Stdio::piped())
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .kill_on_drop(true);
    let mut child = match cmd.spawn() {
        Ok(c) => c,
        Err(e) => {
            return CheckResult {
                name,
                ok: false,
                detail: format!("could not spawn bash -n: {e}"),
            };
        }
    };
    if let Some(mut stdin) = child.stdin.take() {
        use tokio::io::AsyncWriteExt;
        let _ = stdin.write_all(script.as_bytes()).await;
        drop(stdin);
    }
    let result = tokio::time::timeout(Duration::from_secs(5), child.wait_with_output()).await;
    match result {
        Ok(Ok(out)) if out.status.success() => CheckResult {
            name,
            ok: true,
            detail: "parses".into(),
        },
        Ok(Ok(out)) => CheckResult {
            name,
            ok: false,
            detail: format!(
                "bash -n exit {}: {}",
                out.status.code().unwrap_or(-1),
                String::from_utf8_lossy(&out.stderr).trim()
            ),
        },
        Ok(Err(e)) => CheckResult {
            name,
            ok: false,
            detail: format!("io error: {e}"),
        },
        Err(_) => CheckResult {
            name,
            ok: false,
            detail: "bash -n timed out".into(),
        },
    }
}

async fn check_agent_backend(cfg: &ServiceConfig) -> CheckResult {
    match cfg.agent.backend {
        AgentBackend::Codex => check_command_on_path("agent.backend=codex", &cfg.codex.command),
        AgentBackend::ClaudeCode => {
            check_command_on_path("agent.backend=claude_code", &cfg.claude_code.command)
        }
        AgentBackend::OpenAiCompat | AgentBackend::AnthropicMessages => CheckResult {
            name: "agent backend",
            ok: false,
            detail: format!(
                "agent.backend `{}` is in SPEC v2 but not implemented in this build",
                cfg.agent.backend.as_str()
            ),
        },
        AgentBackend::Other(ref k) => CheckResult {
            name: "agent backend",
            ok: false,
            detail: format!("agent.backend `{k}` is not a SPEC v2 value"),
        },
    }
}

fn check_command_on_path(name: &'static str, command: &str) -> CheckResult {
    let first = command.split_whitespace().next().unwrap_or("");
    if first.is_empty() {
        return CheckResult {
            name,
            ok: false,
            detail: "command is empty".into(),
        };
    }
    if path_contains(first) {
        CheckResult {
            name,
            ok: true,
            detail: format!("`{first}` found on PATH"),
        }
    } else {
        CheckResult {
            name,
            ok: false,
            detail: format!("`{first}` not found on PATH"),
        }
    }
}

fn path_contains(name: &str) -> bool {
    if name.contains('/') {
        return Path::new(name).is_file();
    }
    let Some(path) = std::env::var_os("PATH") else {
        return false;
    };
    for dir in std::env::split_paths(&path) {
        let candidate = dir.join(name);
        if candidate.is_file() {
            return true;
        }
    }
    false
}

async fn check_tracker_reachable(cfg: &ServiceConfig) -> CheckResult {
    match cfg.tracker.kind {
        TrackerKind::Linear => {
            let api_key = cfg.tracker.api_key.clone().unwrap_or_default();
            let project_slug = cfg.tracker.project_slug.clone().unwrap_or_default();
            if api_key.is_empty() || project_slug.is_empty() {
                return CheckResult {
                    name: "tracker reachable",
                    ok: false,
                    detail: "missing tracker.api_key or tracker.project_slug".into(),
                };
            }
            let client = match LinearClient::new(LinearConfig {
                endpoint: cfg.tracker.endpoint.clone(),
                api_key,
                project_slug,
                active_states: cfg.tracker.active_states.clone(),
                terminal_states: cfg.tracker.terminal_states.clone(),
            }) {
                Ok(c) => c,
                Err(e) => {
                    return CheckResult {
                        name: "tracker reachable",
                        ok: false,
                        detail: format!("LinearClient::new failed: {e}"),
                    };
                }
            };
            // Time-bound the network call so an unreachable Linear endpoint
            // doesn't hang the doctor.
            match tokio::time::timeout(Duration::from_secs(10), client.fetch_candidate_issues())
                .await
            {
                Ok(Ok(issues)) => CheckResult {
                    name: "tracker reachable",
                    ok: true,
                    detail: format!("Linear returned {} candidate issue(s)", issues.len()),
                },
                Ok(Err(e)) => CheckResult {
                    name: "tracker reachable",
                    ok: false,
                    detail: format!("Linear API error: {e}"),
                },
                Err(_) => CheckResult {
                    name: "tracker reachable",
                    ok: false,
                    detail: "tracker request timed out after 10s".into(),
                },
            }
        }
        TrackerKind::Other(ref k) => CheckResult {
            name: "tracker reachable",
            ok: false,
            detail: format!("unsupported tracker.kind `{k}`"),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::TempDir;

    #[tokio::test]
    async fn fails_when_workflow_missing() {
        let mut buf = Vec::new();
        let n = run(Path::new("/no/such/file.md"), &mut buf).await.unwrap();
        assert!(n >= 1);
        let s = String::from_utf8(buf).unwrap();
        assert!(s.contains("workflow file loadable"));
        assert!(s.contains("✗"));
    }

    #[tokio::test]
    async fn reports_missing_api_key() {
        let tmp = TempDir::new().unwrap();
        let workflow = tmp.path().join("WORKFLOW.md");
        std::fs::write(
            &workflow,
            "---\ntracker:\n  kind: linear\n  project_slug: demo\n---\nbody\n",
        )
        .unwrap();
        let mut buf = Vec::new();
        let n = run(&workflow, &mut buf).await.unwrap();
        assert!(n >= 1);
        let s = String::from_utf8(buf).unwrap();
        assert!(s.contains("dispatch preflight"));
        assert!(s.contains("api_key"));
    }

    #[tokio::test]
    async fn reports_missing_codex_binary() {
        let tmp = TempDir::new().unwrap();
        let workflow = tmp.path().join("WORKFLOW.md");
        let workspace_root = tmp.path().join("ws");
        std::env::set_var("SYMPHONY_DOCTOR_TEST_KEY", "k");
        let body = format!(
            "---\ntracker:\n  kind: linear\n  api_key: $SYMPHONY_DOCTOR_TEST_KEY\n  project_slug: demo\nworkspace:\n  root: {}\nagent:\n  backend: codex\ncodex:\n  command: definitely-not-on-PATH-symphony\n---\nbody\n",
            workspace_root.display()
        );
        std::fs::write(&workflow, body).unwrap();
        let mut buf = Vec::new();
        let _ = run(&workflow, &mut buf).await.unwrap();
        let s = String::from_utf8(buf).unwrap();
        assert!(s.contains("agent.backend=codex"));
        assert!(s.contains("not found on PATH"));
        std::env::remove_var("SYMPHONY_DOCTOR_TEST_KEY");
    }

    #[tokio::test]
    async fn parses_hook_scripts_with_bash() {
        let tmp = TempDir::new().unwrap();
        let workflow = tmp.path().join("WORKFLOW.md");
        let workspace_root = tmp.path().join("ws");
        std::env::set_var("SYMPHONY_DOCTOR_TEST_KEY_HOOK", "k");
        // `if then fi` (missing condition) is an unambiguous bash syntax
        // error that `bash -n` rejects.
        let body = format!(
            "---\ntracker:\n  kind: linear\n  api_key: $SYMPHONY_DOCTOR_TEST_KEY_HOOK\n  project_slug: demo\nworkspace:\n  root: {}\nhooks:\n  before_run: |\n    if then\n      echo bad\n    fi\n---\nbody\n",
            workspace_root.display()
        );
        std::fs::write(&workflow, body).unwrap();
        let mut buf = Vec::new();
        let _ = run(&workflow, &mut buf).await.unwrap();
        let s = String::from_utf8(buf).unwrap();
        assert!(s.contains("hooks.before_run"));
        assert!(s.contains("bash -n exit"));
        std::env::remove_var("SYMPHONY_DOCTOR_TEST_KEY_HOOK");
    }
}
