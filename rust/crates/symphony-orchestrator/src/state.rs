use std::collections::{BTreeMap, BTreeSet, HashMap};
use std::time::Instant;

use symphony_core::Issue;
use time::OffsetDateTime;

/// SPEC §4.1.6 live session metadata.
#[derive(Debug, Clone, Default)]
pub struct LiveSession {
    pub session_id: Option<String>,
    pub thread_id: Option<String>,
    pub turn_id: Option<String>,
    pub agent_runner_pid: Option<String>,
    pub last_agent_event: Option<String>,
    pub last_agent_message: Option<String>,
    pub last_agent_timestamp: Option<OffsetDateTime>,
    /// Monotonic timestamp used by stall detection; not exposed publicly.
    pub last_agent_timestamp_monotonic: Option<Instant>,
    pub agent_input_tokens: u64,
    pub agent_output_tokens: u64,
    pub agent_total_tokens: u64,
    pub last_reported_input_tokens: u64,
    pub last_reported_output_tokens: u64,
    pub last_reported_total_tokens: u64,
    pub turn_count: u32,
}

/// SPEC §16.4 running entry.
#[derive(Debug, Clone)]
pub struct RunningEntry {
    pub identifier: String,
    pub issue: Issue,
    pub session: LiveSession,
    pub retry_attempt: Option<u32>,
    pub started_at: OffsetDateTime,
    pub started_monotonic: Instant,
}

/// SPEC §4.1.7 retry entry.
#[derive(Debug, Clone)]
pub struct RetryEntry {
    pub issue_id: String,
    pub identifier: String,
    pub attempt: u32,
    pub due_at: Instant,
    pub error: Option<String>,
}

#[derive(Debug, Default, Clone)]
pub struct CodexTotals {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
    pub seconds_running: f64,
}

/// SPEC §4.1.8 single-authority orchestrator state.
#[derive(Debug, Default)]
pub struct OrchestratorState {
    pub poll_interval_ms: u64,
    pub max_concurrent_agents: usize,
    pub running: HashMap<String, RunningEntry>,
    pub claimed: BTreeSet<String>,
    pub retry_attempts: BTreeMap<String, RetryEntry>,
    pub completed: BTreeSet<String>,
    pub agent_totals: CodexTotals,
    pub agent_rate_limits: Option<serde_json::Value>,
}
