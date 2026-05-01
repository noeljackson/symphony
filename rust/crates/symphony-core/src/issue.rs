//! SPEC §4.1.1 normalized issue record.

use serde::{Deserialize, Serialize};
use time::OffsetDateTime;

/// Blocker reference (SPEC §4.1.1 `blocked_by`).
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize, Default)]
pub struct Blocker {
    pub id: Option<String>,
    pub identifier: Option<String>,
    pub state: Option<String>,
}

/// Normalized issue record used by orchestration, prompt rendering, and
/// observability output.
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Issue {
    pub id: String,
    pub identifier: String,
    pub title: String,
    pub description: Option<String>,
    pub priority: Option<i32>,
    pub state: String,
    pub branch_name: Option<String>,
    pub url: Option<String>,
    /// Always lowercase per SPEC §11.3.
    pub labels: Vec<String>,
    pub blocked_by: Vec<Blocker>,
    #[serde(with = "time::serde::rfc3339::option")]
    pub created_at: Option<OffsetDateTime>,
    #[serde(with = "time::serde::rfc3339::option")]
    pub updated_at: Option<OffsetDateTime>,
}

impl Issue {
    /// Lower-cased view of `state` for SPEC §4.2 normalized state comparisons.
    pub fn normalized_state(&self) -> String {
        self.state.to_lowercase()
    }

    /// Whether all entries in `blocked_by` reference issues in a terminal
    /// state, taking the configured `terminal_states` (SPEC §8.2 blocker rule).
    pub fn blockers_all_terminal(&self, terminal_states: &[String]) -> bool {
        self.blocked_by.iter().all(|b| match &b.state {
            None => false,
            Some(s) => terminal_states.iter().any(|t| t.eq_ignore_ascii_case(s)),
        })
    }
}
