//! Claude Code stdio backend (SPEC v2 §10.B).
//!
//! Drives the `claude` CLI in stream-json mode:
//! `claude --print --output-format stream-json --input-format stream-json --verbose`
//!
//! The protocol is simpler than Codex's JSON-RPC:
//!
//! - The CLI emits a `system` init message on startup; we extract `session_id`
//!   from it and use that as our `thread_id`.
//! - To run a turn we write a `user` message to stdin and consume stream-json
//!   events until a `result` message arrives.
//! - Tool calls come as `assistant` messages containing `tool_use` blocks; we
//!   reply with `user` messages containing `tool_result` blocks.
//! - The same session stays alive across continuation turns; closing stdin
//!   ends the session.
//!
//! This crate reuses [`symphony_codex::channel::Channel`],
//! [`symphony_codex::events::RuntimeEvent`], and the [`ToolExecutor`] plumbing
//! from `symphony-codex` rather than redeclaring them.

pub mod client;

pub use client::{ClaudeCodeClient, ClaudeCodeLaunch};
