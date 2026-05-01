//! In-memory `Tracker` implementation used by tests and the future
//! orchestrator integration test suite.

use std::sync::Mutex;

use async_trait::async_trait;
use symphony_core::Issue;

use crate::errors::TrackerError;
use crate::tracker::{IssueState, Tracker};

#[derive(Default)]
pub struct MemoryTracker {
    inner: Mutex<MemoryState>,
}

#[derive(Default)]
struct MemoryState {
    issues: Vec<Issue>,
    fail_candidates: Option<TrackerError>,
    fail_refresh: Option<TrackerError>,
}

impl MemoryTracker {
    pub fn with_issues(issues: Vec<Issue>) -> Self {
        Self {
            inner: Mutex::new(MemoryState {
                issues,
                ..Default::default()
            }),
        }
    }

    pub fn replace(&self, issues: Vec<Issue>) {
        self.inner.lock().unwrap().issues = issues;
    }

    pub fn fail_candidates_with(&self, e: TrackerError) {
        self.inner.lock().unwrap().fail_candidates = Some(e);
    }

    pub fn fail_refresh_with(&self, e: TrackerError) {
        self.inner.lock().unwrap().fail_refresh = Some(e);
    }
}

#[async_trait]
impl Tracker for MemoryTracker {
    async fn fetch_candidate_issues(&self) -> Result<Vec<Issue>, TrackerError> {
        let mut guard = self.inner.lock().unwrap();
        if let Some(e) = guard.fail_candidates.take() {
            return Err(e);
        }
        Ok(guard.issues.clone())
    }

    async fn fetch_issues_by_states(
        &self,
        state_names: &[String],
    ) -> Result<Vec<Issue>, TrackerError> {
        let guard = self.inner.lock().unwrap();
        Ok(guard
            .issues
            .iter()
            .filter(|i| state_names.iter().any(|s| s.eq_ignore_ascii_case(&i.state)))
            .cloned()
            .collect())
    }

    async fn fetch_issue_states_by_ids(
        &self,
        issue_ids: &[String],
    ) -> Result<Vec<IssueState>, TrackerError> {
        let mut guard = self.inner.lock().unwrap();
        if let Some(e) = guard.fail_refresh.take() {
            return Err(e);
        }
        let by_id = guard
            .issues
            .iter()
            .filter(|i| issue_ids.iter().any(|id| id == &i.id))
            .cloned();
        let mut out: Vec<IssueState> = by_id
            .map(|i| IssueState {
                id: i.id,
                identifier: i.identifier,
                state: i.state,
            })
            .collect();
        out.sort_by_key(|s| {
            issue_ids
                .iter()
                .position(|id| id == &s.id)
                .unwrap_or(usize::MAX)
        });
        Ok(out)
    }
}
