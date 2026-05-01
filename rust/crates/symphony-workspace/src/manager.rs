//! SPEC §9.1–§9.3 workspace manager.

use std::path::{Path, PathBuf};

use symphony_core::sanitize::workspace_key;

use crate::errors::WorkspaceError;
use crate::hooks::{HookKind, HookRunner};
use crate::path_safety::ensure_within_root;

/// Per-issue workspace metadata (SPEC §4.1.4).
#[derive(Debug, Clone)]
pub struct Workspace {
    pub path: PathBuf,
    pub workspace_key: String,
    pub created_now: bool,
}

/// Owns the workspace root and the hook runner, and exposes the workspace
/// lifecycle operations the orchestrator and worker need.
pub struct WorkspaceManager {
    root: PathBuf,
    hooks: HookRunner,
    after_create: Option<String>,
    before_remove: Option<String>,
}

impl WorkspaceManager {
    pub fn new(
        root: PathBuf,
        hook_timeout_ms: u64,
        after_create: Option<String>,
        before_remove: Option<String>,
    ) -> Self {
        Self {
            root,
            hooks: HookRunner::new(hook_timeout_ms),
            after_create,
            before_remove,
        }
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    /// Create or reuse the per-issue workspace directory. Runs `after_create`
    /// only when the directory was newly created on this call (SPEC §9.2).
    /// Failure of `after_create` is fatal and removes the partial directory
    /// (SPEC §9.4).
    pub async fn ensure_for_issue(&self, identifier: &str) -> Result<Workspace, WorkspaceError> {
        let key = workspace_key(identifier);
        let path = self.root.join(&key);

        // Defensive: the sanitizer should already prevent this, but enforce
        // Invariant 2 explicitly before any filesystem mutation.
        ensure_within_root(&path, &self.root)?;

        // Make the workspace root if it doesn't exist.
        tokio::fs::create_dir_all(&self.root).await?;

        let created_now = match tokio::fs::create_dir(&path).await {
            Ok(()) => true,
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => {
                let meta = tokio::fs::metadata(&path).await?;
                if !meta.is_dir() {
                    return Err(WorkspaceError::Io(std::io::Error::new(
                        std::io::ErrorKind::AlreadyExists,
                        format!(
                            "workspace path exists but is not a directory: {}",
                            path.display()
                        ),
                    )));
                }
                false
            }
            Err(e) => return Err(e.into()),
        };

        if created_now {
            if let Err(e) = self
                .hooks
                .run(HookKind::AfterCreate, self.after_create.as_deref(), &path)
                .await
            {
                // Best-effort cleanup of the partially-prepared directory so
                // the next attempt starts fresh.
                let _ = tokio::fs::remove_dir_all(&path).await;
                return Err(e);
            }
        }

        Ok(Workspace {
            path,
            workspace_key: key,
            created_now,
        })
    }

    /// SPEC §16: workspace path for an identifier without creating anything.
    pub fn path_for(&self, identifier: &str) -> PathBuf {
        self.root.join(workspace_key(identifier))
    }

    /// SPEC §9: remove a workspace. Runs `before_remove` best-effort first.
    /// Missing directories are a no-op.
    pub async fn remove(&self, identifier: &str) -> Result<(), WorkspaceError> {
        let path = self.path_for(identifier);
        ensure_within_root(&path, &self.root)?;

        if !tokio::fs::try_exists(&path).await.unwrap_or(false) {
            return Ok(());
        }

        self.hooks
            .run_best_effort(HookKind::BeforeRemove, self.before_remove.as_deref(), &path)
            .await;
        tokio::fs::remove_dir_all(&path).await?;
        Ok(())
    }

    /// Borrow the hook runner so worker tasks can run `before_run` / `after_run`
    /// against the workspace directly.
    pub fn hooks(&self) -> &HookRunner {
        &self.hooks
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::TempDir;

    fn manager_with(root: &Path) -> WorkspaceManager {
        WorkspaceManager::new(root.to_path_buf(), 60_000, None, None)
    }

    #[tokio::test]
    async fn creates_directory_and_marks_created_now() {
        let root = TempDir::new().unwrap();
        let mgr = manager_with(root.path());
        let ws = mgr.ensure_for_issue("MT-1").await.unwrap();
        assert!(ws.path.is_dir());
        assert_eq!(ws.workspace_key, "MT-1");
        assert!(ws.created_now);
    }

    #[tokio::test]
    async fn reuses_existing_directory() {
        let root = TempDir::new().unwrap();
        let mgr = manager_with(root.path());
        let _ = mgr.ensure_for_issue("MT-2").await.unwrap();
        let ws = mgr.ensure_for_issue("MT-2").await.unwrap();
        assert!(!ws.created_now);
    }

    #[tokio::test]
    async fn sanitizes_identifier() {
        let root = TempDir::new().unwrap();
        let mgr = manager_with(root.path());
        let ws = mgr.ensure_for_issue("a b/c").await.unwrap();
        assert_eq!(ws.workspace_key, "a_b_c");
        assert_eq!(ws.path, root.path().join("a_b_c"));
    }

    #[tokio::test]
    async fn after_create_runs_only_on_new_directory() {
        let root = TempDir::new().unwrap();
        let stamp = root.path().join("stamp");
        let mgr = WorkspaceManager::new(
            root.path().to_path_buf(),
            60_000,
            Some(format!("touch {}", stamp.display())),
            None,
        );
        let _ = mgr.ensure_for_issue("MT-3").await.unwrap();
        assert!(stamp.exists());

        let _ = std::fs::remove_file(&stamp);
        let _ = mgr.ensure_for_issue("MT-3").await.unwrap();
        assert!(
            !stamp.exists(),
            "after_create should not run on existing workspace"
        );
    }

    #[tokio::test]
    async fn after_create_failure_is_fatal_and_cleans_partial_directory() {
        let root = TempDir::new().unwrap();
        let mgr = WorkspaceManager::new(
            root.path().to_path_buf(),
            60_000,
            Some("exit 1".into()),
            None,
        );
        let err = mgr.ensure_for_issue("MT-4").await.unwrap_err();
        assert!(matches!(err, WorkspaceError::HookFailed { .. }));
        assert!(!root.path().join("MT-4").exists());
    }

    #[tokio::test]
    async fn rejects_non_directory_at_workspace_path() {
        let root = TempDir::new().unwrap();
        let path = root.path().join("MT-5");
        tokio::fs::write(&path, b"not a dir".as_slice())
            .await
            .unwrap();
        let mgr = manager_with(root.path());
        let err = mgr.ensure_for_issue("MT-5").await.unwrap_err();
        assert!(matches!(err, WorkspaceError::Io(_)));
    }

    #[tokio::test]
    async fn remove_runs_before_remove_then_deletes() {
        let root = TempDir::new().unwrap();
        let stamp = root.path().join("removed");
        let mgr = WorkspaceManager::new(
            root.path().to_path_buf(),
            60_000,
            None,
            Some(format!("touch {}", stamp.display())),
        );
        let _ = mgr.ensure_for_issue("MT-6").await.unwrap();
        mgr.remove("MT-6").await.unwrap();
        assert!(stamp.exists());
        assert!(!root.path().join("MT-6").exists());
    }

    #[tokio::test]
    async fn remove_swallows_before_remove_failure() {
        let root = TempDir::new().unwrap();
        let mgr = WorkspaceManager::new(
            root.path().to_path_buf(),
            60_000,
            None,
            Some("exit 1".into()),
        );
        let _ = mgr.ensure_for_issue("MT-7").await.unwrap();
        // Failure should be ignored and the directory should still be removed.
        mgr.remove("MT-7").await.unwrap();
        assert!(!root.path().join("MT-7").exists());
    }

    #[tokio::test]
    async fn remove_missing_directory_is_noop() {
        let root = TempDir::new().unwrap();
        let mgr = manager_with(root.path());
        mgr.remove("never-existed").await.unwrap();
    }
}
