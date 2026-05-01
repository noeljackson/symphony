//! Stub for the Linear adapter. Implemented in Phase 3.

use async_trait::async_trait;
use symphony_core::Issue;

use crate::errors::TrackerError;
use crate::tracker::{IssueState, Tracker};

#[derive(Debug, Clone)]
pub struct LinearConfig {
    pub endpoint: String,
    pub api_key: String,
    pub project_slug: String,
    pub active_states: Vec<String>,
    pub terminal_states: Vec<String>,
}

pub struct LinearClient {
    _cfg: LinearConfig,
    _http: reqwest::Client,
}

impl LinearClient {
    pub fn new(cfg: LinearConfig) -> Self {
        let http = reqwest::Client::builder()
            .timeout(std::time::Duration::from_secs(30))
            .build()
            .expect("reqwest client");
        Self { _cfg: cfg, _http: http }
    }
}

#[async_trait]
impl Tracker for LinearClient {
    async fn fetch_candidate_issues(&self) -> Result<Vec<Issue>, TrackerError> {
        Err(TrackerError::LinearApiRequest("not yet implemented".into()))
    }

    async fn fetch_issues_by_states(
        &self,
        _state_names: &[String],
    ) -> Result<Vec<Issue>, TrackerError> {
        Err(TrackerError::LinearApiRequest("not yet implemented".into()))
    }

    async fn fetch_issue_states_by_ids(
        &self,
        _issue_ids: &[String],
    ) -> Result<Vec<IssueState>, TrackerError> {
        Err(TrackerError::LinearApiRequest("not yet implemented".into()))
    }
}
