//! Shared tracker/tool selection for the daemon and doctor.
use crate::{
    linear::{LinearClient, LinearConfig, ReqwestTransport},
    linear_tool::LinearGraphqlTool,
    repository::RepositoryClient,
    Tracker, TrackerError,
};
use std::sync::Arc;
use symphony_codex::tools::{ToolExecutor, UnsupportedToolExecutor};
use symphony_core::config::{TrackerConfig, TrackerKind};

pub type TrackerWithTools = (Arc<dyn Tracker>, Arc<dyn ToolExecutor>);

pub fn build_tracker(config: &TrackerConfig) -> Result<TrackerWithTools, TrackerError> {
    match &config.kind {
        TrackerKind::Linear => {
            let client = LinearClient::new(LinearConfig {
                endpoint: config.endpoint.clone(),
                api_key: config.api_key.clone().unwrap_or_default(),
                project_slug: config.project_slug.clone().unwrap_or_default(),
                active_states: config.active_states.clone(),
                terminal_states: config.terminal_states.clone(),
            })?;
            let transport = Arc::new(ReqwestTransport::new(
                config.endpoint.clone(),
                config.api_key.clone().unwrap_or_default(),
            ));
            Ok((
                Arc::new(client),
                Arc::new(LinearGraphqlTool::new(transport)),
            ))
        }
        TrackerKind::Github | TrackerKind::Forgejo => Ok((
            Arc::new(RepositoryClient::new(config)?),
            Arc::new(UnsupportedToolExecutor),
        )),
        TrackerKind::Other(kind) => Err(TrackerError::UnsupportedTrackerKind(kind.clone())),
    }
}
