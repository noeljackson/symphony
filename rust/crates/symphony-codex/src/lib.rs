//! Codex app-server stdio client. SPEC §10.
//!
//! The wire format is JSON-RPC 2.0-ish over newline-delimited JSON. The
//! transport is abstracted behind [`channel::Channel`] so tests can drive the
//! protocol without spawning a real subprocess.

pub mod channel;
pub mod client;
pub mod errors;
pub mod events;
pub mod tools;

pub use channel::{Channel, ChildChannel, MemoryChannel};
pub use client::{CodexClient, CodexLaunch, SessionPolicies, TurnRequest, TurnSummary};
pub use errors::CodexError;
pub use events::{RuntimeEvent, TokenUsage};
pub use tools::{ToolExecutor, ToolResult, UnsupportedToolExecutor};
