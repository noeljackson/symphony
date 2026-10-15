//! Optional HTTP dashboard + JSON API. SPEC §13.7.

pub mod api;
mod dashboard;
pub mod server;

pub use server::{serve, serve_with_workspace, ServerHandle};
