//! `linear_graphql` client-side tool extension. SPEC §10.5.
//!
//! Lets the coding agent execute one GraphQL operation against the Symphony-
//! configured Linear endpoint with the configured auth, without exposing
//! tokens to the agent.

use std::sync::Arc;

use async_trait::async_trait;
use serde_json::{json, Value};
use symphony_codex::tools::{ToolExecutor, ToolResult};

use crate::linear::GraphqlTransport;

const TOOL_NAME: &str = "linear_graphql";
const TOOL_DESCRIPTION: &str = "Execute one Linear GraphQL operation using the configured Symphony auth. Input: { query: string, variables?: object }.";

pub struct LinearGraphqlTool {
    transport: Arc<dyn GraphqlTransport>,
}

impl LinearGraphqlTool {
    pub fn new(transport: Arc<dyn GraphqlTransport>) -> Self {
        Self { transport }
    }

    fn parse_input(arguments: &Value) -> Result<(String, Value), String> {
        let (query, variables) = match arguments {
            Value::String(s) => (s.clone(), Value::Object(serde_json::Map::new())),
            Value::Object(map) => {
                let q = map
                    .get("query")
                    .and_then(|v| v.as_str())
                    .ok_or_else(|| "missing required `query` field".to_string())?
                    .to_string();
                let v = map
                    .get("variables")
                    .cloned()
                    .unwrap_or(Value::Object(serde_json::Map::new()));
                (q, v)
            }
            _ => return Err("expected object or string input".into()),
        };
        if query.trim().is_empty() {
            return Err("`query` must be non-empty".into());
        }
        if !matches!(variables, Value::Object(_)) {
            return Err("`variables` must be an object when provided".into());
        }
        if count_top_level_operations(&query) > 1 {
            return Err(
                "exactly one GraphQL operation is allowed per linear_graphql tool call".into(),
            );
        }
        Ok((query, variables))
    }
}

#[async_trait]
impl ToolExecutor for LinearGraphqlTool {
    fn specs(&self) -> Vec<Value> {
        vec![json!({
            "name": TOOL_NAME,
            "description": TOOL_DESCRIPTION,
            "parameters": {
                "type": "object",
                "properties": {
                    "query": {"type": "string"},
                    "variables": {"type": "object"}
                },
                "required": ["query"]
            }
        })]
    }

    async fn execute(&self, name: &str, arguments: &Value) -> ToolResult {
        if name != TOOL_NAME {
            return ToolResult::failure(format!("unsupported tool: {name}"));
        }
        let (query, variables) = match Self::parse_input(arguments) {
            Ok(v) => v,
            Err(e) => return ToolResult::failure(e),
        };
        match self.transport.post(&query, variables).await {
            Ok(body) => {
                if body.get("errors").is_some() {
                    ToolResult {
                        success: false,
                        output: body,
                    }
                } else {
                    ToolResult::success(body)
                }
            }
            Err(e) => ToolResult::failure(e.to_string()),
        }
    }
}

fn count_top_level_operations(query: &str) -> usize {
    // Heuristic: count top-level `query`/`mutation`/`subscription` keywords
    // outside of comments and string blocks. The bare `{ ... }` shorthand also
    // counts as one operation.
    let mut depth = 0i32;
    let mut ops = 0usize;
    let mut i = 0usize;
    let bytes = query.as_bytes();
    let mut shorthand_seen = false;
    while i < bytes.len() {
        let b = bytes[i];
        match b {
            b'#' => {
                while i < bytes.len() && bytes[i] != b'\n' {
                    i += 1;
                }
            }
            b'"' => {
                // Skip GraphQL string. Triple-quoted block strings start with """.
                if bytes.get(i..i + 3) == Some(b"\"\"\"") {
                    i += 3;
                    while i + 3 <= bytes.len() && &bytes[i..i + 3] != b"\"\"\"" {
                        i += 1;
                    }
                    i += 3;
                } else {
                    i += 1;
                    while i < bytes.len() && bytes[i] != b'"' {
                        if bytes[i] == b'\\' {
                            i += 2;
                        } else {
                            i += 1;
                        }
                    }
                    i += 1;
                }
            }
            b'{' => {
                if depth == 0 && !shorthand_seen {
                    // Bare top-level `{ ... }` shorthand.
                    let preceded_by_op = preceded_by_operation_keyword(query, i);
                    if !preceded_by_op {
                        ops += 1;
                        shorthand_seen = true;
                    }
                }
                depth += 1;
                i += 1;
            }
            b'}' => {
                depth -= 1;
                i += 1;
            }
            _ if depth == 0 && is_op_keyword_at(query, i) => {
                ops += 1;
                i += operation_keyword_len(query, i);
            }
            _ => i += 1,
        }
    }
    ops
}

fn is_op_keyword_at(s: &str, i: usize) -> bool {
    if i > 0 {
        let prev = s.as_bytes()[i - 1];
        if prev.is_ascii_alphanumeric() || prev == b'_' {
            return false;
        }
    }
    matches!(operation_keyword_len(s, i), 5 | 8 | 12)
}

fn operation_keyword_len(s: &str, i: usize) -> usize {
    let rest = &s[i..];
    if rest.starts_with("query") && !is_ident_continuation(rest, 5) {
        5
    } else if rest.starts_with("mutation") && !is_ident_continuation(rest, 8) {
        8
    } else if rest.starts_with("subscription") && !is_ident_continuation(rest, 12) {
        12
    } else {
        0
    }
}

fn is_ident_continuation(s: &str, off: usize) -> bool {
    s.as_bytes()
        .get(off)
        .map(|b| b.is_ascii_alphanumeric() || *b == b'_')
        .unwrap_or(false)
}

fn preceded_by_operation_keyword(s: &str, brace_idx: usize) -> bool {
    let mut j = brace_idx;
    while j > 0 {
        j -= 1;
        let b = s.as_bytes()[j];
        if !b.is_ascii_whitespace() {
            // Walk back over an identifier (operation name).
            let mut end = j + 1;
            while j > 0 {
                let prev = s.as_bytes()[j - 1];
                if prev.is_ascii_alphanumeric() || prev == b'_' {
                    j -= 1;
                } else {
                    break;
                }
            }
            let word = &s[j..end];
            if matches!(word, "query" | "mutation" | "subscription") {
                return true;
            }
            // Walk back across whitespace before the identifier and try again
            // (handles `query Name {`).
            while j > 0 && s.as_bytes()[j - 1].is_ascii_whitespace() {
                j -= 1;
            }
            end = j;
            while j > 0 {
                let prev = s.as_bytes()[j - 1];
                if prev.is_ascii_alphanumeric() || prev == b'_' {
                    j -= 1;
                } else {
                    break;
                }
            }
            return matches!(&s[j..end], "query" | "mutation" | "subscription");
        }
    }
    false
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::linear::GraphqlTransport;
    use crate::TrackerError;
    use std::sync::Mutex;

    struct EchoTransport {
        calls: Mutex<Vec<(String, Value)>>,
        response: Value,
    }

    #[async_trait]
    impl GraphqlTransport for EchoTransport {
        async fn post(&self, query: &str, variables: Value) -> Result<Value, TrackerError> {
            self.calls
                .lock()
                .unwrap()
                .push((query.to_string(), variables));
            Ok(self.response.clone())
        }
    }

    fn tool(response: Value) -> (LinearGraphqlTool, Arc<EchoTransport>) {
        let echo = Arc::new(EchoTransport {
            calls: Mutex::new(Vec::new()),
            response,
        });
        (LinearGraphqlTool::new(echo.clone()), echo)
    }

    #[tokio::test]
    async fn rejects_unknown_tool_name() {
        let (t, _e) = tool(json!({}));
        let r = t.execute("not_linear_graphql", &json!({})).await;
        assert!(!r.success);
    }

    #[tokio::test]
    async fn requires_query_field() {
        let (t, _e) = tool(json!({}));
        let r = t.execute("linear_graphql", &json!({})).await;
        assert!(!r.success);
    }

    #[tokio::test]
    async fn rejects_blank_query() {
        let (t, _e) = tool(json!({}));
        let r = t.execute("linear_graphql", &json!({"query": "   "})).await;
        assert!(!r.success);
    }

    #[tokio::test]
    async fn rejects_non_object_variables() {
        let (t, _e) = tool(json!({}));
        let r = t
            .execute(
                "linear_graphql",
                &json!({"query": "query { x }", "variables": "nope"}),
            )
            .await;
        assert!(!r.success);
    }

    #[tokio::test]
    async fn rejects_multi_operation_document() {
        let (t, _e) = tool(json!({}));
        let r = t
            .execute(
                "linear_graphql",
                &json!({
                    "query": "query A { x } query B { y }",
                }),
            )
            .await;
        assert!(!r.success);
        let err = r.output.as_str().unwrap_or("");
        assert!(err.contains("exactly one GraphQL operation"));
    }

    #[tokio::test]
    async fn accepts_single_query_and_returns_data() {
        let body = json!({"data": {"viewer": {"id": "u-1"}}});
        let (t, echo) = tool(body.clone());
        let r = t
            .execute(
                "linear_graphql",
                &json!({"query": "query { viewer { id } }"}),
            )
            .await;
        assert!(r.success);
        assert_eq!(r.output, body);
        assert_eq!(echo.calls.lock().unwrap().len(), 1);
    }

    #[tokio::test]
    async fn accepts_shorthand_string_input() {
        let body = json!({"data": {"viewer": {"id": "u-1"}}});
        let (t, _e) = tool(body.clone());
        let r = t
            .execute("linear_graphql", &json!("{ viewer { id } }"))
            .await;
        assert!(r.success);
        assert_eq!(r.output, body);
    }

    #[tokio::test]
    async fn graphql_errors_set_success_false_but_preserve_body() {
        let body = json!({"errors": [{"message": "boom"}]});
        let (t, _e) = tool(body.clone());
        let r = t
            .execute("linear_graphql", &json!({"query": "query { x }"}))
            .await;
        assert!(!r.success);
        assert_eq!(r.output, body);
    }

    #[test]
    fn counts_top_level_operations() {
        assert_eq!(count_top_level_operations("query { x }"), 1);
        assert_eq!(count_top_level_operations("query A { x } query B { y }"), 2);
        assert_eq!(count_top_level_operations("{ x }"), 1);
        assert_eq!(
            count_top_level_operations("query { x query: y }"),
            1,
            "nested `query` field name should not count"
        );
        assert_eq!(
            count_top_level_operations("query A { x } # query B {}\n"),
            1,
            "comments should not count"
        );
    }
}
