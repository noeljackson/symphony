use thiserror::Error;

/// SPEC §10.6 RECOMMENDED normalized categories.
#[derive(Debug, Error)]
pub enum CodexError {
    #[error("codex_not_found: {0}")]
    NotFound(String),
    #[error("invalid_workspace_cwd: {0}")]
    InvalidWorkspaceCwd(String),
    #[error("response_timeout")]
    ResponseTimeout,
    #[error("turn_timeout")]
    TurnTimeout,
    #[error("port_exit: {0}")]
    PortExit(String),
    #[error("response_error: {0}")]
    ResponseError(String),
    #[error("turn_failed: {0}")]
    TurnFailed(String),
    #[error("turn_cancelled")]
    TurnCancelled,
    #[error("turn_input_required")]
    TurnInputRequired,
}
