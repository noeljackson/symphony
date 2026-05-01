//! Trait the orchestrator calls when reconciliation needs to clean up a
//! per-issue workspace (SPEC §8.5 terminal-state branch).
//!
//! Kept as a small trait so the actor itself can stay decoupled from the
//! workspace crate for testing. The CLI plugs in a real implementation that
//! delegates to `WorkspaceManager::remove`.

use async_trait::async_trait;

#[async_trait]
pub trait WorkspaceCleaner: Send + Sync {
    async fn remove(&self, identifier: &str);
}

#[derive(Default)]
pub struct NoopCleaner;

#[async_trait]
impl WorkspaceCleaner for NoopCleaner {
    async fn remove(&self, _identifier: &str) {}
}
