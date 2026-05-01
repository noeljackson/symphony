//! Production [`WorkerRunner`] implementation. Bridges WorkspaceManager,
//! HookRunner, PromptBuilder, the Codex client, and the Tracker into the
//! per-attempt algorithm in SPEC §16.5.

use std::process::Stdio;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use symphony_codex::{
    CodexClient, CodexLaunch, ChildChannel, RuntimeEvent, SessionPolicies, ToolExecutor,
    TurnRequest, UnsupportedToolExecutor,
};
use symphony_core::config::ServiceConfig;
use symphony_core::prompt::PromptBuilder;
use symphony_core::Issue;
use symphony_tracker::Tracker;
use symphony_workspace::{ensure_within_root, HookKind, WorkspaceManager};
use tokio::process::Command;
use tokio::sync::mpsc;

use crate::worker::{WorkerOutcome, WorkerRunner};
use crate::workspace_cleaner::WorkspaceCleaner;

/// Bridge so [`WorkspaceManager`] can be plugged into the orchestrator as
/// a [`WorkspaceCleaner`] without leaking the workspace crate into the
/// actor.
pub struct WorkspaceManagerCleaner {
    pub manager: Arc<WorkspaceManager>,
}

#[async_trait]
impl WorkspaceCleaner for WorkspaceManagerCleaner {
    async fn remove(&self, identifier: &str) {
        if let Err(e) = self.manager.remove(identifier).await {
            tracing::warn!(identifier = %identifier, error = %e, "workspace cleanup failed");
        }
    }
}

fn yaml_to_json(v: &serde_yaml::Value) -> serde_json::Value {
    use serde_json::Value as J;
    use serde_yaml::Value as Y;
    match v {
        Y::Null => J::Null,
        Y::Bool(b) => J::Bool(*b),
        Y::Number(n) => {
            if let Some(i) = n.as_i64() {
                J::from(i)
            } else if let Some(u) = n.as_u64() {
                J::from(u)
            } else if let Some(f) = n.as_f64() {
                serde_json::Number::from_f64(f)
                    .map(J::Number)
                    .unwrap_or(J::Null)
            } else {
                J::Null
            }
        }
        Y::String(s) => J::String(s.clone()),
        Y::Sequence(items) => J::Array(items.iter().map(yaml_to_json).collect()),
        Y::Mapping(map) => {
            let mut out = serde_json::Map::new();
            for (k, val) in map {
                if let Some(key) = k.as_str() {
                    out.insert(key.to_string(), yaml_to_json(val));
                }
            }
            J::Object(out)
        }
        Y::Tagged(t) => yaml_to_json(&t.value),
    }
}

pub struct RealWorker {
    cfg: Arc<ServiceConfig>,
    workspace_mgr: Arc<WorkspaceManager>,
    tracker: Arc<dyn Tracker>,
    prompt_builder: Arc<PromptBuilder>,
    tools: Arc<dyn ToolExecutor>,
}

impl RealWorker {
    pub fn new(
        cfg: Arc<ServiceConfig>,
        workspace_mgr: Arc<WorkspaceManager>,
        tracker: Arc<dyn Tracker>,
        prompt_builder: Arc<PromptBuilder>,
    ) -> Self {
        Self {
            cfg,
            workspace_mgr,
            tracker,
            prompt_builder,
            tools: Arc::new(UnsupportedToolExecutor),
        }
    }

    pub fn with_tools(mut self, tools: Arc<dyn ToolExecutor>) -> Self {
        self.tools = tools;
        self
    }

    fn session_policies(&self) -> SessionPolicies {
        let defaults = SessionPolicies::default();
        SessionPolicies {
            approval_policy: self
                .cfg
                .codex
                .approval_policy
                .as_ref()
                .map(yaml_to_json)
                .unwrap_or(defaults.approval_policy),
            thread_sandbox: self
                .cfg
                .codex
                .thread_sandbox
                .as_ref()
                .map(yaml_to_json)
                .unwrap_or(defaults.thread_sandbox),
            turn_sandbox_policy: self
                .cfg
                .codex
                .turn_sandbox_policy
                .as_ref()
                .map(yaml_to_json)
                .unwrap_or(defaults.turn_sandbox_policy),
        }
    }

    fn is_active(&self, state: &str) -> bool {
        self.cfg
            .tracker
            .active_states
            .iter()
            .any(|s| s.eq_ignore_ascii_case(state))
    }
}

#[async_trait]
impl WorkerRunner for RealWorker {
    async fn run(
        &self,
        issue: Issue,
        attempt: Option<u32>,
        events: mpsc::Sender<RuntimeEvent>,
    ) -> WorkerOutcome {
        let workspace = match self.workspace_mgr.ensure_for_issue(&issue.identifier).await {
            Ok(ws) => ws,
            Err(e) => return WorkerOutcome::Failure { error: format!("workspace: {e}") },
        };

        if let Err(e) = ensure_within_root(&workspace.path, self.workspace_mgr.root()) {
            return WorkerOutcome::Failure { error: format!("invalid_workspace_cwd: {e}") };
        }

        if let Err(e) = self
            .workspace_mgr
            .hooks()
            .run(
                HookKind::BeforeRun,
                self.cfg.hooks.before_run.as_deref(),
                &workspace.path,
            )
            .await
        {
            self.workspace_mgr
                .hooks()
                .run_best_effort(
                    HookKind::AfterRun,
                    self.cfg.hooks.after_run.as_deref(),
                    &workspace.path,
                )
                .await;
            return WorkerOutcome::Failure { error: format!("before_run: {e}") };
        }

        let cwd_str = workspace.path.to_string_lossy().to_string();
        let mut command = Command::new("bash");
        command
            .arg("-lc")
            .arg(&self.cfg.codex.command)
            .current_dir(&workspace.path)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .kill_on_drop(true);

        let child = match command.spawn() {
            Ok(c) => c,
            Err(e) => {
                self.workspace_mgr
                    .hooks()
                    .run_best_effort(
                        HookKind::AfterRun,
                        self.cfg.hooks.after_run.as_deref(),
                        &workspace.path,
                    )
                    .await;
                return WorkerOutcome::Failure {
                    error: format!("codex_not_found: {e}"),
                };
            }
        };

        let channel = match ChildChannel::new(child) {
            Ok(c) => c,
            Err(e) => {
                self.workspace_mgr
                    .hooks()
                    .run_best_effort(
                        HookKind::AfterRun,
                        self.cfg.hooks.after_run.as_deref(),
                        &workspace.path,
                    )
                    .await;
                return WorkerOutcome::Failure {
                    error: format!("codex spawn: {e}"),
                };
            }
        };

        let launch = CodexLaunch {
            workspace: workspace.path.clone(),
            policies: self.session_policies(),
            read_timeout: Duration::from_millis(self.cfg.codex.read_timeout_ms),
            turn_timeout: Duration::from_millis(self.cfg.codex.turn_timeout_ms),
        };
        let mut client = CodexClient::new(channel, events.clone(), launch).with_tools(self.tools.clone());

        if let Err(e) = client.start_session(&cwd_str).await {
            client.stop_session().await;
            self.workspace_mgr
                .hooks()
                .run_best_effort(
                    HookKind::AfterRun,
                    self.cfg.hooks.after_run.as_deref(),
                    &workspace.path,
                )
                .await;
            return WorkerOutcome::Failure { error: format!("startup_failed: {e}") };
        }

        let mut current_issue = issue.clone();
        let mut turn_number: u32 = 1;
        let outcome = loop {
            let prompt_attempt = if turn_number > 1 {
                Some(turn_number)
            } else {
                attempt
            };
            let prompt = match self.prompt_builder.render(&current_issue, prompt_attempt) {
                Ok(p) => p,
                Err(e) => break WorkerOutcome::Failure { error: format!("prompt: {e}") },
            };
            let title = format!("{}: {}", current_issue.identifier, current_issue.title);
            let req = TurnRequest { prompt, title };
            if let Err(e) = client.run_turn(req, &cwd_str).await {
                break WorkerOutcome::Failure { error: format!("turn_failed: {e}") };
            }

            // SPEC §16.5: re-check tracker state to decide whether to start
            // another continuation turn.
            match self
                .tracker
                .fetch_issue_states_by_ids(&[current_issue.id.clone()])
                .await
            {
                Ok(states) => {
                    if let Some(state) = states.into_iter().next() {
                        current_issue.state = state.state;
                    }
                }
                Err(e) => {
                    break WorkerOutcome::Failure {
                        error: format!("issue refresh failed: {e}"),
                    };
                }
            }
            if !self.is_active(&current_issue.state) {
                break WorkerOutcome::Success;
            }
            if turn_number >= self.cfg.agent.max_turns {
                break WorkerOutcome::Success;
            }
            turn_number = turn_number.saturating_add(1);
        };

        client.stop_session().await;
        self.workspace_mgr
            .hooks()
            .run_best_effort(
                HookKind::AfterRun,
                self.cfg.hooks.after_run.as_deref(),
                &workspace.path,
            )
            .await;
        outcome
    }
}
