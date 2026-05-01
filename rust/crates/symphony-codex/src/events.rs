use serde::{Deserialize, Serialize};
use serde_json::Value;
use time::OffsetDateTime;

/// SPEC §10.4 emitted runtime events. We model the core variants the
/// orchestrator cares about as a closed enum and pass through everything
/// else as `OtherMessage` / `Notification`.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RuntimeEvent {
    pub event: String,
    #[serde(with = "time::serde::rfc3339")]
    pub timestamp: OffsetDateTime,
    pub agent_runner_pid: Option<String>,
    pub usage: Option<TokenUsage>,
    /// Absolute thread totals when the upstream payload provides them
    /// (`thread/tokenUsage/updated`); separate from `usage` so the
    /// orchestrator can distinguish absolute vs delta sources per SPEC §13.5.
    pub thread_total_usage: Option<TokenUsage>,
    pub session_id: Option<String>,
    pub thread_id: Option<String>,
    pub turn_id: Option<String>,
    /// Raw payload for downstream observers / dashboards.
    #[serde(default)]
    pub payload: Value,
    pub message: Option<String>,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct TokenUsage {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
}

impl RuntimeEvent {
    pub fn new(event: impl Into<String>) -> Self {
        Self {
            event: event.into(),
            timestamp: OffsetDateTime::now_utc(),
            agent_runner_pid: None,
            usage: None,
            thread_total_usage: None,
            session_id: None,
            thread_id: None,
            turn_id: None,
            payload: Value::Null,
            message: None,
        }
    }
}
