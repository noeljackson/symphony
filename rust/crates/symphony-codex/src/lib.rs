//! Codex app-server stdio client. SPEC §10. Phase 4 fills this in.

pub mod errors;
pub mod events;

pub use errors::CodexError;
pub use events::RuntimeEvent;
