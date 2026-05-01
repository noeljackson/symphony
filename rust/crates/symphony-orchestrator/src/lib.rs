//! Orchestrator actor. SPEC §7, §8, §16.
//!
//! The orchestrator is the only component that mutates scheduling state. We
//! model it as a Tokio actor: a single task owns [`OrchestratorState`] and
//! receives [`OrchestratorCommand`]s through an mpsc channel. Worker tasks
//! and retry timers are spawned as children and report back via the same
//! channel.

pub mod actor;
pub mod dispatch;
pub mod pricing;
pub mod runner;
pub mod state;
pub mod worker;

pub use actor::{
    EventBroadcast, ForceRetryOutcome, Orchestrator, OrchestratorCommand, OrchestratorHandle,
    Snapshot, SnapshotRetryRow, SnapshotRunningRow, EVENT_BROADCAST_CAPACITY,
};
pub use dispatch::{
    dispatch_eligibility, sort_for_dispatch, DispatchEligibility, EligibilityVerdict,
};
pub use pricing::{builtin_price_table, ModelPrice, PriceTable, TokenUsage};
pub use runner::{RealWorker, WorkspaceManagerCleaner};
pub use state::{
    AgentTotals, LiveSession, OrchestratorState, RecentEvent, RetryEntry, RunningEntry,
    RECENT_EVENTS_CAP,
};
pub use worker::{WorkerExit, WorkerOutcome, WorkerRunner};
pub use workspace_cleaner::{NoopCleaner, WorkspaceCleaner};

pub mod workspace_cleaner;
