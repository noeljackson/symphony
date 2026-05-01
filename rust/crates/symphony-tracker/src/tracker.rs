use async_trait::async_trait;
use symphony_core::Issue;

use crate::errors::TrackerError;

/// Minimal issue-state record returned by reconciliation refresh
/// (`fetch_issue_states_by_ids`).
#[derive(Debug, Clone)]
pub struct IssueState {
    pub id: String,
    pub identifier: String,
    pub state: String,
}

/// SPEC §11.1 required tracker adapter operations.
#[async_trait]
pub trait Tracker: Send + Sync {
    /// Return issues currently in `active_states` for the configured project.
    async fn fetch_candidate_issues(&self) -> Result<Vec<Issue>, TrackerError>;

    /// Return issues whose state matches any of `state_names`. Used for the
    /// startup terminal-workspace cleanup sweep.
    async fn fetch_issues_by_states(
        &self,
        state_names: &[String],
    ) -> Result<Vec<Issue>, TrackerError>;

    /// Refresh state for a specific list of issue IDs. Used by reconciliation.
    async fn fetch_issue_states_by_ids(
        &self,
        issue_ids: &[String],
    ) -> Result<Vec<IssueState>, TrackerError>;
}
