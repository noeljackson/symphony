use serde::{Deserialize, Serialize};
use time::OffsetDateTime;

/// SPEC §10.4 emitted runtime events.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RuntimeEvent {
    pub event: String,
    #[serde(with = "time::serde::rfc3339")]
    pub timestamp: OffsetDateTime,
    pub codex_app_server_pid: Option<String>,
    pub usage: Option<TokenUsage>,
    #[serde(default)]
    pub payload: serde_json::Value,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct TokenUsage {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
}
