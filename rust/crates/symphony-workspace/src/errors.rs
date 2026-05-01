use thiserror::Error;

#[derive(Debug, Error)]
pub enum WorkspaceError {
    #[error("workspace path `{0}` is outside workspace root")]
    OutsideRoot(String),
    #[error("workspace io error: {0}")]
    Io(#[from] std::io::Error),
    #[error("hook `{name}` failed: {reason}")]
    HookFailed { name: &'static str, reason: String },
    #[error("hook `{0}` timed out")]
    HookTimeout(&'static str),
}
