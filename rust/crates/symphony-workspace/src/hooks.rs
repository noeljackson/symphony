//! SPEC §9.4 workspace hooks.
//!
//! Hooks are arbitrary shell scripts read from `WORKFLOW.md`. We execute them
//! via `bash -lc <script>` with the workspace as `cwd`, enforce
//! `hooks.timeout_ms`, capture (and truncate) stdout/stderr, and return a
//! typed [`HookOutcome`] so callers can apply the per-hook failure semantics:
//!
//! | Hook            | On failure / timeout                       |
//! |-----------------|--------------------------------------------|
//! | `after_create`  | fatal — workspace creation aborts          |
//! | `before_run`    | fatal — current run attempt aborts         |
//! | `after_run`     | logged, ignored                            |
//! | `before_remove` | logged, ignored, cleanup proceeds          |

use std::path::Path;
use std::process::Stdio;
use std::time::Duration;

use tokio::io::AsyncReadExt;
use tokio::process::Command;
use tokio::time::timeout;

use crate::errors::WorkspaceError;

const LOG_OUTPUT_BYTE_CAP: usize = 4096;

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum HookKind {
    AfterCreate,
    BeforeRun,
    AfterRun,
    BeforeRemove,
}

impl HookKind {
    pub const fn name(&self) -> &'static str {
        match self {
            HookKind::AfterCreate => "after_create",
            HookKind::BeforeRun => "before_run",
            HookKind::AfterRun => "after_run",
            HookKind::BeforeRemove => "before_remove",
        }
    }

    /// SPEC §9.4: which hooks are best-effort (failures logged + ignored).
    pub const fn is_best_effort(&self) -> bool {
        matches!(self, HookKind::AfterRun | HookKind::BeforeRemove)
    }
}

#[derive(Debug)]
pub struct HookOutcome {
    pub kind: HookKind,
    pub status: i32,
    pub stdout_truncated: String,
    pub stderr_truncated: String,
}

pub struct HookRunner {
    timeout_ms: u64,
}

impl HookRunner {
    pub fn new(timeout_ms: u64) -> Self {
        Self { timeout_ms }
    }

    /// Execute `script` via `bash -lc` with `cwd = workspace`. Returns
    /// `Ok(None)` when `script` is `None` (hook not configured).
    pub async fn run(
        &self,
        kind: HookKind,
        script: Option<&str>,
        workspace: &Path,
    ) -> Result<Option<HookOutcome>, WorkspaceError> {
        let Some(script) = script else {
            return Ok(None);
        };
        if script.trim().is_empty() {
            return Ok(None);
        }

        tracing::info!(
            hook = kind.name(),
            cwd = %workspace.display(),
            "starting hook"
        );

        let mut cmd = Command::new("bash");
        cmd.arg("-lc")
            .arg(script)
            .current_dir(workspace)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .kill_on_drop(true);
        let mut child = cmd
            .spawn()
            .map_err(|e| WorkspaceError::HookFailed {
                name: kind.name(),
                reason: format!("spawn failed: {e}"),
            })?;

        let mut stdout = child.stdout.take();
        let mut stderr = child.stderr.take();
        let dur = Duration::from_millis(self.timeout_ms);

        let mut out_buf = Vec::new();
        let mut err_buf = Vec::new();
        let drain = async {
            if let Some(s) = stdout.as_mut() {
                let _ = s.read_to_end(&mut out_buf).await;
            }
            if let Some(s) = stderr.as_mut() {
                let _ = s.read_to_end(&mut err_buf).await;
            }
        };

        let result = timeout(dur, async {
            drain.await;
            child.wait().await
        })
        .await;

        match result {
            Err(_elapsed) => {
                let _ = child.start_kill();
                let _ = child.wait().await;
                tracing::warn!(hook = kind.name(), timeout_ms = self.timeout_ms, "hook timed out");
                return Err(WorkspaceError::HookTimeout(kind.name()));
            }
            Ok(Err(e)) => {
                return Err(WorkspaceError::HookFailed {
                    name: kind.name(),
                    reason: e.to_string(),
                });
            }
            Ok(Ok(status)) => {
                let stdout_truncated = truncate_lossy(&out_buf, LOG_OUTPUT_BYTE_CAP);
                let stderr_truncated = truncate_lossy(&err_buf, LOG_OUTPUT_BYTE_CAP);
                let code = status.code().unwrap_or(-1);
                if status.success() {
                    return Ok(Some(HookOutcome {
                        kind,
                        status: code,
                        stdout_truncated,
                        stderr_truncated,
                    }));
                } else {
                    tracing::warn!(
                        hook = kind.name(),
                        code,
                        stderr = %stderr_truncated,
                        "hook exited non-zero"
                    );
                    return Err(WorkspaceError::HookFailed {
                        name: kind.name(),
                        reason: format!("exit code {code}"),
                    });
                }
            }
        }
    }

    /// Run a best-effort hook (`after_run`, `before_remove`). Failures and
    /// timeouts are logged and swallowed per SPEC §9.4.
    pub async fn run_best_effort(
        &self,
        kind: HookKind,
        script: Option<&str>,
        workspace: &Path,
    ) {
        debug_assert!(kind.is_best_effort());
        if let Err(e) = self.run(kind, script, workspace).await {
            tracing::warn!(hook = kind.name(), error = %e, "best-effort hook failed");
        }
    }
}

fn truncate_lossy(buf: &[u8], max: usize) -> String {
    if buf.len() <= max {
        return String::from_utf8_lossy(buf).into_owned();
    }
    let mut s = String::from_utf8_lossy(&buf[..max]).into_owned();
    s.push_str("…<truncated>");
    s
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::TempDir;

    #[tokio::test]
    async fn returns_none_when_script_missing() {
        let runner = HookRunner::new(60_000);
        let dir = TempDir::new().unwrap();
        let out = runner.run(HookKind::AfterCreate, None, dir.path()).await.unwrap();
        assert!(out.is_none());
    }

    #[tokio::test]
    async fn returns_none_for_blank_script() {
        let runner = HookRunner::new(60_000);
        let dir = TempDir::new().unwrap();
        let out = runner
            .run(HookKind::AfterCreate, Some("   \n\t"), dir.path())
            .await
            .unwrap();
        assert!(out.is_none());
    }

    #[tokio::test]
    async fn runs_in_workspace_cwd() {
        let runner = HookRunner::new(60_000);
        let dir = TempDir::new().unwrap();
        let out = runner
            .run(HookKind::AfterCreate, Some("pwd"), dir.path())
            .await
            .unwrap()
            .unwrap();
        // macOS may prepend `/private/...` to /tmp paths; just compare canonical.
        let expected = dir.path().canonicalize().unwrap();
        let printed = std::path::PathBuf::from(out.stdout_truncated.trim()).canonicalize().unwrap();
        assert_eq!(printed, expected);
    }

    #[tokio::test]
    async fn surfaces_non_zero_exit_as_hook_failed() {
        let runner = HookRunner::new(60_000);
        let dir = TempDir::new().unwrap();
        let err = runner
            .run(HookKind::BeforeRun, Some("exit 3"), dir.path())
            .await
            .unwrap_err();
        match err {
            WorkspaceError::HookFailed { name, reason } => {
                assert_eq!(name, "before_run");
                assert!(reason.contains("exit code 3"));
            }
            other => panic!("unexpected error: {other:?}"),
        }
    }

    #[tokio::test]
    async fn enforces_timeout() {
        let runner = HookRunner::new(150);
        let dir = TempDir::new().unwrap();
        let err = runner
            .run(HookKind::AfterCreate, Some("sleep 5"), dir.path())
            .await
            .unwrap_err();
        assert!(matches!(err, WorkspaceError::HookTimeout("after_create")));
    }

    #[tokio::test]
    async fn best_effort_swallows_failures() {
        let runner = HookRunner::new(60_000);
        let dir = TempDir::new().unwrap();
        // Should not panic / propagate.
        runner
            .run_best_effort(HookKind::AfterRun, Some("exit 1"), dir.path())
            .await;
        runner
            .run_best_effort(HookKind::BeforeRemove, Some("sleep 5"), dir.path())
            .await;
    }
}
