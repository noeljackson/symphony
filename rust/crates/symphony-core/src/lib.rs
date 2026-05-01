//! Symphony core: domain model, workflow loader, typed config, prompt
//! rendering, and workflow file watcher. Implements SPEC §4–§6 and §12.

pub mod config;
pub mod errors;
pub mod issue;
pub mod prompt;
pub mod sanitize;
pub mod watcher;
pub mod workflow;

pub use config::ServiceConfig;
pub use errors::{ConfigError, PromptError, WorkflowError};
pub use issue::{Blocker, Issue};
pub use prompt::PromptBuilder;
pub use workflow::{WorkflowDefinition, WorkflowLoader};
