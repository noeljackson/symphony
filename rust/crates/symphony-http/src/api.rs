//! JSON shapes for `/api/v1/*` (SPEC §13.7.2).

use serde::{Deserialize, Serialize};
use serde_json::Value;
use symphony_orchestrator::{Snapshot, SnapshotRetryRow, SnapshotRunningRow};
use time::format_description::well_known::Rfc3339;
use time::OffsetDateTime;

#[derive(Debug, Serialize)]
pub struct StateView {
    pub generated_at: String,
    pub counts: Counts,
    pub running: Vec<RunningRowView>,
    pub retrying: Vec<RetryRowView>,
    pub codex_totals: TotalsView,
    pub rate_limits: Option<Value>,
}

#[derive(Debug, Serialize)]
pub struct Counts {
    pub running: usize,
    pub retrying: usize,
}

#[derive(Debug, Serialize)]
pub struct RunningRowView {
    pub issue_id: String,
    pub issue_identifier: String,
    pub state: String,
    pub session_id: Option<String>,
    pub turn_count: u32,
    pub last_event: Option<String>,
    pub last_message: Option<String>,
    pub started_at: String,
    pub last_event_at: Option<String>,
    pub tokens: TokenView,
}

#[derive(Debug, Serialize)]
pub struct RetryRowView {
    pub issue_id: String,
    pub issue_identifier: String,
    pub attempt: u32,
    pub due_at: String,
    pub error: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct TokenView {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
}

#[derive(Debug, Serialize)]
pub struct TotalsView {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
    pub seconds_running: f64,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct ApiError {
    pub error: ApiErrorBody,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct ApiErrorBody {
    pub code: String,
    pub message: String,
}

impl StateView {
    pub fn from_snapshot(snap: &Snapshot, rate_limits: Option<Value>) -> Self {
        let now_str = snap
            .generated_at
            .format(&Rfc3339)
            .unwrap_or_else(|_| String::from("now"));
        Self {
            generated_at: now_str,
            counts: Counts {
                running: snap.running.len(),
                retrying: snap.retrying.len(),
            },
            running: snap.running.iter().map(running_row).collect(),
            retrying: snap.retrying.iter().map(retry_row).collect(),
            codex_totals: TotalsView {
                input_tokens: snap.codex_totals.input_tokens,
                output_tokens: snap.codex_totals.output_tokens,
                total_tokens: snap.codex_totals.total_tokens,
                seconds_running: snap.codex_totals.seconds_running,
            },
            rate_limits,
        }
    }
}

fn running_row(r: &SnapshotRunningRow) -> RunningRowView {
    RunningRowView {
        issue_id: r.issue_id.clone(),
        issue_identifier: r.identifier.clone(),
        state: r.state.clone(),
        session_id: r.session_id.clone(),
        turn_count: r.turn_count,
        last_event: r.last_event.clone(),
        last_message: r.last_message.clone(),
        started_at: format_iso(r.started_at),
        last_event_at: r.last_event_at.map(format_iso),
        tokens: TokenView {
            input_tokens: r.input_tokens,
            output_tokens: r.output_tokens,
            total_tokens: r.total_tokens,
        },
    }
}

fn retry_row(r: &SnapshotRetryRow) -> RetryRowView {
    let due_at = (OffsetDateTime::now_utc() + time::Duration::milliseconds(r.due_in_ms.max(0)))
        .format(&Rfc3339)
        .unwrap_or_default();
    RetryRowView {
        issue_id: r.issue_id.clone(),
        issue_identifier: r.identifier.clone(),
        attempt: r.attempt,
        due_at,
        error: r.error.clone(),
    }
}

fn format_iso(t: OffsetDateTime) -> String {
    t.format(&Rfc3339).unwrap_or_default()
}

pub fn issue_view(snap: &Snapshot, identifier: &str) -> Option<Value> {
    if let Some(running) = snap
        .running
        .iter()
        .find(|r| r.identifier.eq_ignore_ascii_case(identifier))
    {
        return Some(serde_json::json!({
            "issue_identifier": running.identifier,
            "issue_id": running.issue_id,
            "status": "running",
            "running": running_row(running),
            "retry": serde_json::Value::Null,
        }));
    }
    if let Some(retry) = snap
        .retrying
        .iter()
        .find(|r| r.identifier.eq_ignore_ascii_case(identifier))
    {
        return Some(serde_json::json!({
            "issue_identifier": retry.identifier,
            "issue_id": retry.issue_id,
            "status": "retrying",
            "running": serde_json::Value::Null,
            "retry": retry_row(retry),
        }));
    }
    None
}
