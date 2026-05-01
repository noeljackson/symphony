//! Dynamic tool dispatch contract used by `item/tool/call` handling.
//!
//! Symphony advertises a small set of optional client-side tools (e.g.
//! `linear_graphql`, see SPEC §10.5). When the agent invokes a tool, we ask a
//! [`ToolExecutor`] to handle it. Unsupported tool names MUST still return a
//! failure result to prevent the session from stalling.

use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ToolResult {
    pub success: bool,
    pub output: Value,
}

impl ToolResult {
    pub fn success(output: Value) -> Self {
        Self {
            success: true,
            output,
        }
    }

    pub fn failure(message: impl Into<String>) -> Self {
        Self {
            success: false,
            output: Value::String(message.into()),
        }
    }
}

#[async_trait]
pub trait ToolExecutor: Send + Sync {
    /// Tool specs to advertise during `thread/start`. Defaults to none.
    fn specs(&self) -> Vec<Value> {
        Vec::new()
    }

    async fn execute(&self, name: &str, arguments: &Value) -> ToolResult;
}

/// Default executor that rejects every tool name. Implementations that ship
/// no client-side tools still need to provide an executor so we always reply
/// with a failure (SPEC §10.5).
pub struct UnsupportedToolExecutor;

#[async_trait]
impl ToolExecutor for UnsupportedToolExecutor {
    async fn execute(&self, name: &str, _arguments: &Value) -> ToolResult {
        ToolResult::failure(format!("unsupported tool: {name}"))
    }
}
