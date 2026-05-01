//! Workspace manager + hook runner. SPEC §9.

pub mod errors;
pub mod hooks;
pub mod manager;
pub mod path_safety;

pub use errors::WorkspaceError;
pub use hooks::{HookKind, HookOutcome, HookRunner};
pub use manager::{Workspace, WorkspaceManager};
pub use path_safety::{ensure_within_root, is_within_root};
