use std::collections::{BTreeMap, BTreeSet, HashMap, VecDeque};
use std::time::Instant;

use symphony_core::Issue;
use time::{Date, OffsetDateTime};

/// SPEC v2 §13.7.2: ring-buffer cap for `recent_events`. Operators see the
/// most recent agent activity at this depth; older events are dropped.
pub const RECENT_EVENTS_CAP: usize = 50;

/// SPEC v2 §13.7.2: one entry in the per-issue `recent_events` ring buffer.
#[derive(Debug, Clone)]
pub struct RecentEvent {
    pub at: OffsetDateTime,
    pub event: String,
    pub message: Option<String>,
}

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
    /// SPEC v2 §13.5: model identifier surfaced by the backend. Used as the
    /// price-table key when computing `cost_usd`. Stays `None` for backends
    /// that don't expose the model name on their event stream (today: codex,
    /// claude_code).
    pub model: Option<String>,
    /// SPEC v2 §13.7.2: ring buffer of recent agent events for this session.
    /// Capped at [`RECENT_EVENTS_CAP`]; oldest entries are dropped first.
    /// Cleared on session restart (see RunningEntry construction).
    pub recent_events: VecDeque<RecentEvent>,
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

/// SPEC v2 §13.3 / §13.5: aggregate token, runtime, and USD-cost totals.
///
/// `cost_usd` and `cost_usd_today` are `None` when the implementation cannot
/// price the configured backend (subscription-based agents, or models missing
/// from the price table). Per SPEC §13.5 a `None` cost MUST disable
/// budget-cap enforcement.
#[derive(Debug, Default, Clone)]
pub struct AgentTotals {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
    pub seconds_running: f64,
    pub cost_usd: Option<f64>,
    pub cost_usd_today: Option<f64>,
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
    pub agent_totals: AgentTotals,
    pub agent_rate_limits: Option<serde_json::Value>,
    /// SPEC v2 §13.5: UTC date that `cost_usd_today` is anchored to. Lazy
    /// rollover: when this date != current UTC date, the daily counter
    /// resets to `Some(0.0)` (or `None` if pricing remains unknown).
    pub daily_cost_window: Option<Date>,
    /// One-shot warning suppression: highest budget threshold (in percent)
    /// for which a warning has been emitted for the current daily window.
    /// Reset alongside `daily_cost_window` on day rollover.
    pub last_budget_warning_pct: Option<u32>,
}

/// SPEC v2 §13.5: lazy daily rollover. Resets `cost_usd_today` to
/// `Some(0.0)` (or `None` if pricing has never produced a cost) and clears
/// the warning suppressor whenever the active window date is stale.
pub fn roll_over_daily_cost(state: &mut OrchestratorState, today: Date) {
    match state.daily_cost_window {
        Some(d) if d == today => {}
        _ => {
            state.daily_cost_window = Some(today);
            state.agent_totals.cost_usd_today = if state.agent_totals.cost_usd.is_some() {
                Some(0.0)
            } else {
                None
            };
            state.last_budget_warning_pct = None;
        }
    }
}

/// SPEC v2 §13.5: roll over first, then accumulate the delta against both
/// the lifetime and daily counters. Always promotes `None` totals to
/// `Some(0.0 + delta)` because the caller has just produced a real cost.
pub fn add_cost(state: &mut OrchestratorState, delta_usd: f64, today: Date) {
    roll_over_daily_cost(state, today);
    let lifetime = state.agent_totals.cost_usd.unwrap_or(0.0) + delta_usd;
    let today_total = state.agent_totals.cost_usd_today.unwrap_or(0.0) + delta_usd;
    state.agent_totals.cost_usd = Some(lifetime);
    state.agent_totals.cost_usd_today = Some(today_total);
}

/// SPEC v2 §13.5 / §16.2: budget gate. Returns `true` when dispatch must
/// be skipped because the cumulative daily cost has reached the cap.
/// Returns `false` (cap inert) when either the cap or `cost_usd_today` is
/// unset, per §13.5 ("`null` cost MUST disable budget-cap enforcement").
pub fn budget_cap_reached(state: &OrchestratorState, daily_budget_usd: Option<f64>) -> bool {
    match (daily_budget_usd, state.agent_totals.cost_usd_today) {
        (Some(cap), Some(today)) => today >= cap,
        _ => false,
    }
}

/// SPEC v2 §13.7.2: append `entry` to a ring buffer capped at
/// [`RECENT_EVENTS_CAP`], dropping the oldest entry when full.
pub fn push_recent_event(buf: &mut VecDeque<RecentEvent>, entry: RecentEvent) {
    if buf.len() == RECENT_EVENTS_CAP {
        buf.pop_front();
    }
    buf.push_back(entry);
}

#[cfg(test)]
mod tests {
    use super::*;
    use time::macros::date;

    #[test]
    fn add_cost_rolls_over_daily_window() {
        let mut s = OrchestratorState::default();
        add_cost(&mut s, 0.50, date!(2026 - 5 - 1));
        assert_eq!(s.agent_totals.cost_usd, Some(0.50));
        assert_eq!(s.agent_totals.cost_usd_today, Some(0.50));

        // Same day: accumulates.
        add_cost(&mut s, 0.25, date!(2026 - 5 - 1));
        assert_eq!(s.agent_totals.cost_usd, Some(0.75));
        assert_eq!(s.agent_totals.cost_usd_today, Some(0.75));

        // Next day: lifetime persists, daily resets to 0.0 + delta.
        add_cost(&mut s, 0.10, date!(2026 - 5 - 2));
        assert_eq!(s.agent_totals.cost_usd, Some(0.85));
        assert_eq!(s.agent_totals.cost_usd_today, Some(0.10));
    }

    #[test]
    fn rollover_keeps_today_none_when_pricing_unknown() {
        let mut s = OrchestratorState::default();
        roll_over_daily_cost(&mut s, date!(2026 - 5 - 1));
        assert_eq!(s.daily_cost_window, Some(date!(2026 - 5 - 1)));
        assert_eq!(s.agent_totals.cost_usd_today, None);
    }

    #[test]
    fn budget_cap_inert_without_pricing() {
        let mut s = OrchestratorState::default();
        roll_over_daily_cost(&mut s, date!(2026 - 5 - 1));
        // cost_usd_today is None -> cap is inert per SPEC §13.5.
        assert!(!budget_cap_reached(&s, Some(1.0)));
    }

    #[test]
    fn budget_cap_blocks_at_exact_threshold() {
        let mut s = OrchestratorState::default();
        add_cost(&mut s, 1.0, date!(2026 - 5 - 1));
        assert!(budget_cap_reached(&s, Some(1.0)));
        assert!(budget_cap_reached(&s, Some(0.5)));
        assert!(!budget_cap_reached(&s, Some(1.5)));
        assert!(!budget_cap_reached(&s, None));
    }

    #[test]
    fn rollover_clears_warning_suppressor() {
        let mut s = OrchestratorState::default();
        add_cost(&mut s, 0.50, date!(2026 - 5 - 1));
        s.last_budget_warning_pct = Some(80);
        roll_over_daily_cost(&mut s, date!(2026 - 5 - 2));
        assert_eq!(s.last_budget_warning_pct, None);
    }

    #[test]
    fn push_recent_event_caps_at_50_and_drops_oldest() {
        let mut buf = VecDeque::new();
        for i in 0..(RECENT_EVENTS_CAP + 5) {
            push_recent_event(
                &mut buf,
                RecentEvent {
                    at: time::macros::datetime!(2026-05-01 00:00 UTC),
                    event: format!("ev-{i}"),
                    message: None,
                },
            );
        }
        assert_eq!(buf.len(), RECENT_EVENTS_CAP);
        // The oldest 5 should have been dropped, so the front is "ev-5".
        assert_eq!(buf.front().unwrap().event, "ev-5");
        assert_eq!(
            buf.back().unwrap().event,
            format!("ev-{}", RECENT_EVENTS_CAP + 4)
        );
    }
}
