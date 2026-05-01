//! SPEC §9.5 safety invariants.
//!
//! Workspaces MUST stay under the configured workspace root. Both paths are
//! normalized first; if either leg can be canonicalized (i.e. exists), the
//! canonical form is preferred so that symlinks cannot escape the root.

use std::path::{Component, Path, PathBuf};

use crate::errors::WorkspaceError;

/// Returns `true` iff `candidate` resolves to a path under `root`.
pub fn is_within_root(candidate: &Path, root: &Path) -> bool {
    let candidate = best_effort_canonicalize(candidate);
    let root = best_effort_canonicalize(root);
    candidate.starts_with(&root)
}

/// Reject paths that escape the workspace root (Invariant 2).
pub fn ensure_within_root(candidate: &Path, root: &Path) -> Result<(), WorkspaceError> {
    if is_within_root(candidate, root) {
        Ok(())
    } else {
        Err(WorkspaceError::OutsideRoot(candidate.display().to_string()))
    }
}

fn best_effort_canonicalize(p: &Path) -> PathBuf {
    if let Ok(real) = p.canonicalize() {
        return real;
    }
    // Walk up the path until we find an ancestor that exists, canonicalize
    // it, and reattach the remainder. This lets us validate paths that have
    // not been created yet without losing symlink resolution for the parts
    // that already exist on disk.
    let mut tail: Vec<std::ffi::OsString> = Vec::new();
    let mut cursor = p.to_path_buf();
    let existing = loop {
        if cursor.exists() {
            break Some(cursor.canonicalize().unwrap_or(cursor));
        }
        match cursor.file_name() {
            Some(name) => tail.push(name.to_os_string()),
            None => break None,
        }
        if !cursor.pop() {
            break None;
        }
    };
    let mut out = existing.unwrap_or_else(|| normalize(p));
    for name in tail.into_iter().rev() {
        out.push(name);
    }
    out
}

fn normalize(p: &Path) -> PathBuf {
    let mut out = PathBuf::new();
    for comp in p.components() {
        match comp {
            Component::ParentDir => {
                out.pop();
            }
            Component::CurDir => {}
            other => out.push(other.as_os_str()),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use tempfile::TempDir;

    #[test]
    fn allows_paths_under_root() {
        let root = TempDir::new().unwrap();
        let inside = root.path().join("issue-1");
        assert!(is_within_root(&inside, root.path()));
    }

    #[test]
    fn rejects_sibling_paths() {
        let root = TempDir::new().unwrap();
        let parent = root.path().parent().unwrap();
        let outside = parent.join("escape-attempt");
        assert!(!is_within_root(&outside, root.path()));
    }

    #[test]
    fn rejects_parent_traversal() {
        let root = TempDir::new().unwrap();
        let escaped = root.path().join("..").join("etc");
        assert!(!is_within_root(&escaped, root.path()));
    }

    #[test]
    fn ensure_within_root_returns_typed_error() {
        let root = TempDir::new().unwrap();
        let parent = root.path().parent().unwrap();
        let err = ensure_within_root(&parent.join("nope"), root.path()).unwrap_err();
        assert!(matches!(err, WorkspaceError::OutsideRoot(_)));
    }
}
