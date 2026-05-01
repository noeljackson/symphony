//! Optional HTTP dashboard + JSON API. SPEC §13.7.

pub mod api;
pub mod server;

pub use server::{serve, ServerHandle};
