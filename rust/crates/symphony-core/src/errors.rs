use thiserror::Error;

/// SPEC §5.5 workflow error surface.
#[derive(Debug, Error)]
pub enum WorkflowError {
    #[error("missing_workflow_file: {0}")]
    MissingWorkflowFile(String),
    #[error("workflow_parse_error: {0}")]
    WorkflowParseError(String),
    #[error("workflow_front_matter_not_a_map")]
    WorkflowFrontMatterNotAMap,
    #[error("io error: {0}")]
    Io(#[from] std::io::Error),
}

/// Errors raised while coercing the front-matter map into a typed
/// `ServiceConfig` (SPEC §6).
#[derive(Debug, Error)]
pub enum ConfigError {
    #[error("invalid value for `{field}`: {reason}")]
    InvalidValue { field: String, reason: String },
    #[error("missing required field `{0}`")]
    Missing(&'static str),
    #[error("unsupported tracker.kind `{0}`")]
    UnsupportedTrackerKind(String),
    #[error("missing tracker.api_key (after $VAR resolution)")]
    MissingTrackerApiKey,
    #[error("missing tracker.project_slug")]
    MissingTrackerProjectSlug,
    #[error("codex.command must not be empty")]
    EmptyCodexCommand,
    /// SPEC v2: backend is in the spec set but this implementation does not
    /// ship it.
    #[error("agent.backend `{0}` is defined by SPEC v2 but not implemented in this build")]
    UnimplementedAgentBackend(String),
    /// SPEC v2: backend value is not in the spec set at all.
    #[error("agent.backend `{0}` is not a SPEC v2 backend (expected one of `codex`, `claude_code`, `openai_compat`, `anthropic_messages`)")]
    UnsupportedAgentBackend(String),
    #[error("workflow error: {0}")]
    Workflow(#[from] WorkflowError),
}

/// SPEC §5.5 / §12.4 prompt rendering errors.
#[derive(Debug, Error)]
pub enum PromptError {
    #[error("template_parse_error: {0}")]
    Parse(String),
    #[error("template_render_error: {0}")]
    Render(String),
}
