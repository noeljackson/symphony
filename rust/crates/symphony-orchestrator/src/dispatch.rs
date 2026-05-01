//! Pure helpers for dispatch eligibility, sorting, and concurrency. These
//! are isolated from the actor so they're easy to unit-test.

use std::collections::HashMap;

use symphony_core::config::ServiceConfig;
use symphony_core::Issue;
use time::OffsetDateTime;

use crate::state::OrchestratorState;

/// SPEC §8.2 sort order: priority ascending (null last), created_at oldest
/// first, identifier lexicographic tiebreak.
pub fn sort_for_dispatch(issues: &mut [Issue]) {
    issues.sort_by_key(dispatch_key);
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct DispatchEligibility {
    pub eligible: bool,
    pub reason: EligibilityVerdict,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum EligibilityVerdict {
    Ok,
    AlreadyRunning,
    AlreadyClaimed,
    NotInActiveStates,
    InTerminalStates,
    BlockedByOpenBlocker,
    GlobalSlotsExhausted,
    PerStateSlotsExhausted,
    MissingFields,
}

pub fn dispatch_eligibility(
    issue: &Issue,
    cfg: &ServiceConfig,
    state: &OrchestratorState,
) -> DispatchEligibility {
    if issue.id.is_empty()
        || issue.identifier.is_empty()
        || issue.title.is_empty()
        || issue.state.is_empty()
    {
        return DispatchEligibility {
            eligible: false,
            reason: EligibilityVerdict::MissingFields,
        };
    }
    let state_lower = issue.normalized_state();
    let in_terminal = cfg
        .tracker
        .terminal_states
        .iter()
        .any(|s| s.eq_ignore_ascii_case(&issue.state));
    if in_terminal {
        return verdict(EligibilityVerdict::InTerminalStates);
    }
    let in_active = cfg
        .tracker
        .active_states
        .iter()
        .any(|s| s.eq_ignore_ascii_case(&issue.state));
    if !in_active {
        return verdict(EligibilityVerdict::NotInActiveStates);
    }
    if state.running.contains_key(&issue.id) {
        return verdict(EligibilityVerdict::AlreadyRunning);
    }
    if state.claimed.contains(&issue.id) {
        return verdict(EligibilityVerdict::AlreadyClaimed);
    }
    if state_lower == "todo" && !issue.blockers_all_terminal(&cfg.tracker.terminal_states) {
        return verdict(EligibilityVerdict::BlockedByOpenBlocker);
    }
    if global_available_slots(cfg, state) == 0 {
        return verdict(EligibilityVerdict::GlobalSlotsExhausted);
    }
    if per_state_available_slots(&issue.state, cfg, state) == 0 {
        return verdict(EligibilityVerdict::PerStateSlotsExhausted);
    }
    verdict(EligibilityVerdict::Ok)
}

fn verdict(reason: EligibilityVerdict) -> DispatchEligibility {
    DispatchEligibility {
        eligible: matches!(reason, EligibilityVerdict::Ok),
        reason,
    }
}

pub fn global_available_slots(cfg: &ServiceConfig, state: &OrchestratorState) -> usize {
    cfg.agent
        .max_concurrent_agents
        .saturating_sub(state.running.len())
}

pub fn per_state_available_slots(
    target_state: &str,
    cfg: &ServiceConfig,
    state: &OrchestratorState,
) -> usize {
    let key = target_state.to_lowercase();
    let cap = match cfg.agent.max_concurrent_agents_by_state.get(&key) {
        Some(n) => *n,
        None => cfg.agent.max_concurrent_agents,
    };
    let count = count_running_in_state(target_state, state);
    cap.saturating_sub(count)
}

fn count_running_in_state(target_state: &str, state: &OrchestratorState) -> usize {
    state
        .running
        .values()
        .filter(|r| r.issue.state.eq_ignore_ascii_case(target_state))
        .count()
}

fn dispatch_key(i: &Issue) -> (u8, i32, OffsetDateTime, String) {
    let (bucket, priority) = match i.priority {
        Some(p) => (0, p),
        None => (1, 0),
    };
    let created = i.created_at.unwrap_or(OffsetDateTime::UNIX_EPOCH);
    (bucket, priority, created, i.identifier.clone())
}

/// SPEC §8.4 retry backoff. Continuation retries (after a clean worker exit)
/// use a short fixed delay; failure-driven retries use exponential backoff
/// capped by `agent.max_retry_backoff_ms`.
pub fn retry_delay_ms(attempt: u32, max_cap_ms: u64, continuation: bool) -> u64 {
    if continuation {
        return 1_000;
    }
    let exp_steps = attempt.saturating_sub(1).min(20); // protect against overflow
    let raw = 10_000u64.saturating_mul(1u64 << exp_steps);
    raw.min(max_cap_ms)
}

/// Group running issues by state for snapshot/observability output.
pub fn running_count_by_state(state: &OrchestratorState) -> HashMap<String, usize> {
    let mut counts = HashMap::new();
    for r in state.running.values() {
        *counts.entry(r.issue.state.clone()).or_insert(0) += 1;
    }
    counts
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_yaml::Mapping;
    use std::path::PathBuf;
    use symphony_core::config::{
        AgentConfig, CodexConfig, HooksConfig, PollingConfig, ServerConfig, TrackerConfig,
        TrackerKind, WorkspaceConfig,
    };
    use symphony_core::issue::{Blocker, Issue};
    use time::macros::datetime;

    fn cfg(max_concurrent: usize, by_state: &[(&str, usize)]) -> ServiceConfig {
        let mut by_state_map = std::collections::BTreeMap::new();
        for (k, v) in by_state {
            by_state_map.insert(k.to_string(), *v);
        }
        ServiceConfig {
            tracker: TrackerConfig {
                kind: TrackerKind::Linear,
                endpoint: "https://example".into(),
                api_key: Some("k".into()),
                project_slug: Some("demo".into()),
                active_states: vec!["Todo".into(), "In Progress".into()],
                terminal_states: vec!["Done".into(), "Cancelled".into()],
            },
            polling: PollingConfig {
                interval_ms: 30_000,
            },
            workspace: WorkspaceConfig {
                root: PathBuf::from("/tmp/symphony"),
            },
            hooks: HooksConfig {
                timeout_ms: 60_000,
                ..Default::default()
            },
            agent: AgentConfig {
                backend: symphony_core::config::AgentBackend::Codex,
                max_concurrent_agents: max_concurrent,
                max_turns: 20,
                max_retry_backoff_ms: 300_000,
                max_concurrent_agents_by_state: by_state_map,
            },
            codex: CodexConfig {
                command: "codex".into(),
                approval_policy: None,
                thread_sandbox: None,
                turn_sandbox_policy: None,
                turn_timeout_ms: 3_600_000,
                read_timeout_ms: 5_000,
                stall_timeout_ms: 300_000,
            },
            server: ServerConfig { port: None },
            raw: Mapping::new(),
            workflow_path: PathBuf::from("/tmp/WORKFLOW.md"),
        }
    }

    fn issue(
        id: &str,
        identifier: &str,
        priority: Option<i32>,
        state: &str,
        created_at: Option<time::OffsetDateTime>,
    ) -> Issue {
        Issue {
            id: id.into(),
            identifier: identifier.into(),
            title: "t".into(),
            description: None,
            priority,
            state: state.into(),
            branch_name: None,
            url: None,
            labels: vec![],
            blocked_by: vec![],
            created_at,
            updated_at: None,
        }
    }

    #[test]
    fn sorts_by_priority_then_created_then_identifier() {
        let mut issues = vec![
            issue(
                "c",
                "MT-3",
                None,
                "Todo",
                Some(datetime!(2026-01-01 00:00 UTC)),
            ),
            issue(
                "a",
                "MT-1",
                Some(2),
                "Todo",
                Some(datetime!(2026-01-02 00:00 UTC)),
            ),
            issue(
                "b",
                "MT-2",
                Some(2),
                "Todo",
                Some(datetime!(2026-01-01 00:00 UTC)),
            ),
            issue(
                "d",
                "MT-4",
                Some(1),
                "Todo",
                Some(datetime!(2026-01-03 00:00 UTC)),
            ),
        ];
        sort_for_dispatch(&mut issues);
        assert_eq!(
            issues
                .iter()
                .map(|i| i.identifier.clone())
                .collect::<Vec<_>>(),
            vec!["MT-4", "MT-2", "MT-1", "MT-3"]
        );
    }

    #[test]
    fn todo_with_open_blocker_is_ineligible() {
        let cfg = cfg(10, &[]);
        let mut iss = issue("a", "MT-1", Some(1), "Todo", None);
        iss.blocked_by = vec![Blocker {
            id: Some("b".into()),
            identifier: Some("MT-99".into()),
            state: Some("In Progress".into()),
        }];
        let v = dispatch_eligibility(&iss, &cfg, &OrchestratorState::default());
        assert_eq!(v.reason, EligibilityVerdict::BlockedByOpenBlocker);
    }

    #[test]
    fn todo_with_terminal_blocker_is_eligible() {
        let cfg = cfg(10, &[]);
        let mut iss = issue("a", "MT-1", Some(1), "Todo", None);
        iss.blocked_by = vec![Blocker {
            id: Some("b".into()),
            identifier: Some("MT-99".into()),
            state: Some("Done".into()),
        }];
        let v = dispatch_eligibility(&iss, &cfg, &OrchestratorState::default());
        assert!(v.eligible);
    }

    #[test]
    fn terminal_state_is_ineligible_even_when_listed_active() {
        let cfg = cfg(10, &[]);
        let iss = issue("a", "MT-1", Some(1), "Done", None);
        let v = dispatch_eligibility(&iss, &cfg, &OrchestratorState::default());
        assert_eq!(v.reason, EligibilityVerdict::InTerminalStates);
    }

    #[test]
    fn global_slot_exhaustion_blocks_dispatch() {
        let cfg = cfg(0, &[]);
        let iss = issue("a", "MT-1", Some(1), "Todo", None);
        let v = dispatch_eligibility(&iss, &cfg, &OrchestratorState::default());
        assert_eq!(v.reason, EligibilityVerdict::GlobalSlotsExhausted);
    }

    #[test]
    fn per_state_cap_overrides_global_cap() {
        let cfg = cfg(10, &[("in progress", 0)]);
        let iss = issue("a", "MT-1", Some(1), "In Progress", None);
        let v = dispatch_eligibility(&iss, &cfg, &OrchestratorState::default());
        assert_eq!(v.reason, EligibilityVerdict::PerStateSlotsExhausted);
    }

    #[test]
    fn continuation_uses_one_second_delay() {
        assert_eq!(retry_delay_ms(1, 300_000, true), 1_000);
        assert_eq!(retry_delay_ms(5, 300_000, true), 1_000);
    }

    #[test]
    fn failure_backoff_doubles_and_caps() {
        assert_eq!(retry_delay_ms(1, 300_000, false), 10_000);
        assert_eq!(retry_delay_ms(2, 300_000, false), 20_000);
        assert_eq!(retry_delay_ms(3, 300_000, false), 40_000);
        assert_eq!(retry_delay_ms(10, 300_000, false), 300_000);
    }
}
