use thiserror::Error;

/// SPEC §11.4 RECOMMENDED tracker error categories.
#[derive(Debug, Error)]
pub enum TrackerError {
    #[error("{0}_config: {1}")]
    RepositoryConfig(&'static str, String),
    #[error("{0}_api_request")]
    RepositoryApiRequest(&'static str),
    #[error("{0}_api_status: {1}")]
    RepositoryApiStatus(&'static str, u16),
    #[error("{0}_unknown_payload: {1}")]
    RepositoryUnknownPayload(&'static str, &'static str),
    #[error("{0}_auth: {1}")]
    RepositoryAuth(&'static str, &'static str),
    #[error("github_auth: {0}")]
    GithubAuth(&'static str),
    #[error("unsupported_tracker_kind: {0}")]
    UnsupportedTrackerKind(String),
    #[error("missing_tracker_api_key")]
    MissingTrackerApiKey,
    #[error("missing_tracker_project_slug")]
    MissingTrackerProjectSlug,
    #[error("linear_api_request: {0}")]
    LinearApiRequest(String),
    #[error("linear_api_status: {0}")]
    LinearApiStatus(u16),
    #[error("linear_graphql_errors: {0}")]
    LinearGraphqlErrors(String),
    #[error("linear_unknown_payload: {0}")]
    LinearUnknownPayload(String),
    #[error("linear_missing_end_cursor")]
    LinearMissingEndCursor,
}
