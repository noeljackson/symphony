//! Worker abstraction. The orchestrator does not run codex / workspace logic
//! itself; it spawns a [`WorkerRunner`] per dispatched attempt. A real
//! implementation will be wired up in Phase 6 from `WorkspaceManager`,
//! `CodexClient`, the prompt builder, and the tracker. Tests drive the
//! orchestrator with a scripted runner.

use async_trait::async_trait;
use symphony_codex::RuntimeEvent;
use symphony_core::Issue;
use tokio::sync::mpsc;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum WorkerOutcome {
    /// SPEC §7.1: a successful worker exit triggers a short continuation
    /// retry so the orchestrator can re-check the issue state.
    Success,
    /// Failure / timeout / cancellation — feeds exponential retry backoff.
    Failure { error: String },
    /// SPEC §8.5 stall handling: the orchestrator pulled the plug.
    Cancelled { reason: String },
}

#[derive(Debug)]
pub struct WorkerExit {
    pub issue_id: String,
    pub outcome: WorkerOutcome,
}

#[async_trait]
pub trait WorkerRunner: Send + Sync + 'static {
    /// Run the full agent attempt for `issue` with the given retry/continuation
    /// `attempt` value. Codex updates SHOULD be pushed onto `events` as they
    /// happen so the orchestrator can update LiveSession state and stall
    /// timestamps.
    async fn run(
        &self,
        issue: Issue,
        attempt: Option<u32>,
        events: mpsc::Sender<RuntimeEvent>,
    ) -> WorkerOutcome;
}
