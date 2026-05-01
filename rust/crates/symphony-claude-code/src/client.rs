//! `ClaudeCodeClient`: per-issue session driver for the Claude Code CLI.
//!
//! Lifecycle:
//!
//! 1. `new(...)` accepts a [`Channel`] (stdio or test in-memory) and a
//!    [`ClaudeCodeLaunch`] config snapshot.
//! 2. `start_session(&workspace_cwd)` waits for the CLI's `system`/`init`
//!    bootstrap message and returns its `session_id` as our thread id. No
//!    request is sent — the CLI emits the init message automatically.
//! 3. `run_turn(prompt, &workspace_cwd)` writes one user message to stdin,
//!    streams stream-json events to the orchestrator until a `result`
//!    message arrives, dispatches `tool_use` blocks via the configured
//!    [`ToolExecutor`], and resolves to success/failure based on the
//!    result's `subtype`.
//! 4. `stop_session()` drops stdin so the CLI exits cleanly.

use std::sync::Arc;
use std::time::{Duration, Instant};

use serde_json::{json, Value};
use symphony_codex::channel::Channel;
use symphony_codex::errors::CodexError;
use symphony_codex::events::{RuntimeEvent, TokenUsage};
use symphony_codex::tools::{ToolExecutor, ToolResult, UnsupportedToolExecutor};
use tokio::sync::mpsc;

#[derive(Debug, Clone)]
pub struct ClaudeCodeLaunch {
    pub workspace: std::path::PathBuf,
    pub read_timeout: Duration,
    pub turn_timeout: Duration,
}

pub struct ClaudeCodeClient {
    channel: Arc<dyn Channel>,
    events: mpsc::Sender<RuntimeEvent>,
    tools: Arc<dyn ToolExecutor>,
    read_timeout: Duration,
    turn_timeout: Duration,
    thread_id: Option<String>,
    turn_counter: u32,
    last_thread_total_usage: Option<TokenUsage>,
}

impl ClaudeCodeClient {
    pub fn new(
        channel: Arc<dyn Channel>,
        events: mpsc::Sender<RuntimeEvent>,
        launch: ClaudeCodeLaunch,
    ) -> Self {
        Self {
            channel,
            events,
            tools: Arc::new(UnsupportedToolExecutor),
            read_timeout: launch.read_timeout,
            turn_timeout: launch.turn_timeout,
            thread_id: None,
            turn_counter: 0,
            last_thread_total_usage: None,
        }
    }

    pub fn with_tools(mut self, tools: Arc<dyn ToolExecutor>) -> Self {
        self.tools = tools;
        self
    }

    /// Wait for the CLI's `system`/`init` bootstrap message and return its
    /// `session_id` (used as our `thread_id`).
    pub async fn start_session(&mut self, _workspace_cwd: &str) -> Result<String, CodexError> {
        let deadline = Instant::now() + self.read_timeout;
        loop {
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return Err(CodexError::ResponseTimeout);
            }
            let line = self.channel.recv_line(remaining).await?;
            let trimmed = line.trim();
            if trimmed.is_empty() {
                continue;
            }
            let parsed: Value = match serde_json::from_str(trimmed) {
                Ok(v) => v,
                Err(_) => continue,
            };
            let kind = parsed.get("type").and_then(|v| v.as_str()).unwrap_or("");
            let subtype = parsed.get("subtype").and_then(|v| v.as_str()).unwrap_or("");
            if kind == "system" && subtype == "init" {
                let session_id = parsed
                    .get("session_id")
                    .and_then(|v| v.as_str())
                    .ok_or_else(|| {
                        CodexError::ResponseError("missing system/init session_id".into())
                    })?
                    .to_string();
                self.thread_id = Some(session_id.clone());
                return Ok(session_id);
            }
            // Pre-init noise (rare). Ignore and keep waiting.
        }
    }

    /// Run one turn end-to-end. Streams `RuntimeEvent`s and resolves on the
    /// next `result` message.
    pub async fn run_turn(
        &mut self,
        request: TurnRequest,
        workspace_cwd: &str,
    ) -> Result<TurnSummary, CodexError> {
        let thread_id = self
            .thread_id
            .clone()
            .ok_or_else(|| CodexError::ResponseError("session not started".into()))?;

        self.turn_counter = self.turn_counter.saturating_add(1);
        let turn_id = self.turn_counter.to_string();
        let session_id = format!("{thread_id}-{turn_id}");

        // Send the user message that initiates this turn.
        self.send(&json!({
            "type": "user",
            "message": {
                "role": "user",
                "content": [{ "type": "text", "text": request.prompt }],
            },
            "parent_tool_use_id": null,
            "session_id": thread_id,
        }))
        .await?;

        let mut started = RuntimeEvent::new("session_started");
        started.session_id = Some(session_id.clone());
        started.thread_id = Some(thread_id.clone());
        started.turn_id = Some(turn_id.clone());
        let _ = self.events.send(started).await;

        let _ = workspace_cwd; // workspace cwd is enforced via process cwd / --add-dir
        self.drive_turn(&session_id, &thread_id, &turn_id).await?;
        Ok(TurnSummary {
            thread_id,
            turn_id,
            session_id,
            thread_total_usage: self.last_thread_total_usage.clone(),
        })
    }

    pub async fn stop_session(&mut self) {
        self.channel.close().await;
        self.thread_id = None;
    }

    async fn drive_turn(
        &mut self,
        session_id: &str,
        thread_id: &str,
        turn_id: &str,
    ) -> Result<(), CodexError> {
        let deadline = Instant::now() + self.turn_timeout;
        loop {
            let remaining = deadline.saturating_duration_since(Instant::now());
            if remaining.is_zero() {
                return Err(CodexError::TurnTimeout);
            }
            let read_window = remaining.min(self.read_timeout);
            let line = match self.channel.recv_line(read_window).await {
                Ok(line) => line,
                Err(CodexError::ResponseTimeout) => {
                    if Instant::now() >= deadline {
                        return Err(CodexError::TurnTimeout);
                    }
                    continue;
                }
                Err(e) => return Err(e),
            };
            let trimmed = line.trim();
            if trimmed.is_empty() {
                continue;
            }
            let parsed: Value = match serde_json::from_str(trimmed) {
                Ok(v) => v,
                Err(_) => {
                    let mut ev = RuntimeEvent::new("malformed");
                    ev.session_id = Some(session_id.to_string());
                    ev.payload = Value::String(trimmed.to_string());
                    let _ = self.events.send(ev).await;
                    continue;
                }
            };
            if let Some(done) = self
                .handle_message(parsed, session_id, thread_id, turn_id)
                .await?
            {
                return done;
            }
        }
    }

    async fn handle_message(
        &mut self,
        msg: Value,
        session_id: &str,
        thread_id: &str,
        turn_id: &str,
    ) -> Result<Option<Result<(), CodexError>>, CodexError> {
        let kind = msg
            .get("type")
            .and_then(|v| v.as_str())
            .unwrap_or("")
            .to_string();
        match kind.as_str() {
            "result" => {
                self.merge_result_usage(&msg);
                let subtype = msg
                    .get("subtype")
                    .and_then(|v| v.as_str())
                    .unwrap_or("")
                    .to_string();
                let mut ev = self.event_for("result", &msg, session_id, thread_id, turn_id);
                let outcome = match subtype.as_str() {
                    "success" => {
                        ev.event = "turn_completed".into();
                        let _ = self.events.send(ev).await;
                        Ok(())
                    }
                    _ => {
                        ev.event = "turn_failed".into();
                        ev.message = Some(format!("claude_code result subtype `{subtype}`"));
                        let _ = self.events.send(ev).await;
                        Err(CodexError::TurnFailed(format!(
                            "claude_code result subtype `{subtype}`"
                        )))
                    }
                };
                Ok(Some(outcome))
            }
            "assistant" => {
                self.merge_assistant_usage(&msg);
                if self
                    .dispatch_tool_calls(&msg, session_id, thread_id, turn_id)
                    .await?
                {
                    return Ok(None);
                }
                let mut ev = self.event_for("assistant", &msg, session_id, thread_id, turn_id);
                ev.event = "notification".into();
                ev.message = extract_assistant_text(&msg);
                let _ = self.events.send(ev).await;
                Ok(None)
            }
            "user" => {
                let mut ev = self.event_for("user", &msg, session_id, thread_id, turn_id);
                ev.event = "notification".into();
                let _ = self.events.send(ev).await;
                Ok(None)
            }
            "system" => {
                let mut ev = self.event_for("system", &msg, session_id, thread_id, turn_id);
                ev.event = "notification".into();
                let _ = self.events.send(ev).await;
                Ok(None)
            }
            _ => {
                let mut ev = self.event_for(&kind, &msg, session_id, thread_id, turn_id);
                ev.event = "other_message".into();
                let _ = self.events.send(ev).await;
                Ok(None)
            }
        }
    }

    /// Walk an assistant message's content blocks and dispatch any
    /// `tool_use` entries. Returns `true` if any tool was dispatched (so the
    /// caller can skip the duplicate notification emit).
    async fn dispatch_tool_calls(
        &mut self,
        msg: &Value,
        session_id: &str,
        thread_id: &str,
        turn_id: &str,
    ) -> Result<bool, CodexError> {
        let content = match msg.pointer("/message/content").and_then(|v| v.as_array()) {
            Some(arr) => arr,
            None => return Ok(false),
        };
        let mut dispatched = false;
        let mut tool_results: Vec<Value> = Vec::new();
        for block in content {
            if block.get("type").and_then(|v| v.as_str()) != Some("tool_use") {
                continue;
            }
            dispatched = true;
            let id = block
                .get("id")
                .and_then(|v| v.as_str())
                .unwrap_or("")
                .to_string();
            let name = block
                .get("name")
                .and_then(|v| v.as_str())
                .unwrap_or("")
                .to_string();
            let input = block.get("input").cloned().unwrap_or(Value::Null);
            let result = if name.is_empty() {
                ToolResult::failure("missing tool name")
            } else {
                self.tools.execute(&name, &input).await
            };
            let event_kind = if result.success {
                "tool_call_completed"
            } else if name.is_empty() {
                "unsupported_tool_call"
            } else {
                "tool_call_failed"
            };
            let mut ev = self.event_for("tool_use", block, session_id, thread_id, turn_id);
            ev.event = event_kind.into();
            let _ = self.events.send(ev).await;

            tool_results.push(json!({
                "type": "tool_result",
                "tool_use_id": id,
                "content": [{ "type": "text", "text": render_tool_output(&result.output) }],
                "is_error": !result.success,
            }));
        }

        if !tool_results.is_empty() {
            self.send(&json!({
                "type": "user",
                "message": {
                    "role": "user",
                    "content": tool_results,
                },
                "parent_tool_use_id": null,
                "session_id": thread_id,
            }))
            .await?;
        }
        Ok(dispatched)
    }

    fn event_for(
        &self,
        kind: &str,
        msg: &Value,
        session_id: &str,
        thread_id: &str,
        turn_id: &str,
    ) -> RuntimeEvent {
        let mut ev = RuntimeEvent::new(kind);
        ev.session_id = Some(session_id.to_string());
        ev.thread_id = Some(thread_id.to_string());
        ev.turn_id = Some(turn_id.to_string());
        ev.thread_total_usage = self.last_thread_total_usage.clone();
        ev.payload = msg.clone();
        ev
    }

    fn merge_assistant_usage(&mut self, msg: &Value) {
        if let Some(usage) = msg.pointer("/message/usage") {
            if let Some(t) = parse_usage(usage) {
                self.last_thread_total_usage = Some(t);
            }
        }
    }

    fn merge_result_usage(&mut self, msg: &Value) {
        if let Some(usage) = msg.get("usage") {
            if let Some(t) = parse_usage(usage) {
                self.last_thread_total_usage = Some(t);
            }
        }
    }

    async fn send(&self, payload: &Value) -> Result<(), CodexError> {
        let line = serde_json::to_string(payload)
            .map_err(|e| CodexError::ResponseError(format!("encode: {e}")))?;
        self.channel.send_line(&line).await
    }
}

#[derive(Debug, Clone)]
pub struct TurnRequest {
    pub prompt: String,
    pub title: String,
}

#[derive(Debug, Clone)]
pub struct TurnSummary {
    pub thread_id: String,
    pub turn_id: String,
    pub session_id: String,
    pub thread_total_usage: Option<TokenUsage>,
}

fn extract_assistant_text(msg: &Value) -> Option<String> {
    let content = msg.pointer("/message/content")?.as_array()?;
    let mut buf = String::new();
    for block in content {
        if block.get("type").and_then(|v| v.as_str()) == Some("text") {
            if let Some(t) = block.get("text").and_then(|v| v.as_str()) {
                if !buf.is_empty() {
                    buf.push('\n');
                }
                buf.push_str(t);
            }
        }
    }
    if buf.is_empty() {
        None
    } else {
        Some(buf)
    }
}

fn parse_usage(v: &Value) -> Option<TokenUsage> {
    let input = pick_u64(v, &["input_tokens", "inputTokens"])?;
    let output = pick_u64(v, &["output_tokens", "outputTokens"])?;
    let total = pick_u64(v, &["total_tokens", "totalTokens"]).unwrap_or(input + output);
    Some(TokenUsage {
        input_tokens: input,
        output_tokens: output,
        total_tokens: total,
    })
}

fn pick_u64(v: &Value, keys: &[&str]) -> Option<u64> {
    for k in keys {
        if let Some(n) = v.get(*k).and_then(|v| v.as_u64()) {
            return Some(n);
        }
    }
    None
}

fn render_tool_output(value: &Value) -> String {
    match value {
        Value::String(s) => s.clone(),
        other => other.to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use symphony_codex::channel::MemoryChannel;
    use tokio::sync::mpsc::UnboundedReceiver;

    fn launch() -> ClaudeCodeLaunch {
        ClaudeCodeLaunch {
            workspace: std::path::PathBuf::from("/tmp/issue-1"),
            read_timeout: Duration::from_millis(500),
            turn_timeout: Duration::from_secs(2),
        }
    }

    async fn setup() -> (
        ClaudeCodeClient,
        mpsc::Receiver<RuntimeEvent>,
        UnboundedReceiver<String>,
        mpsc::UnboundedSender<String>,
    ) {
        let (chan, server_inbox, server_outbox) = MemoryChannel::pair();
        let (events_tx, events_rx) = mpsc::channel(64);
        let client = ClaudeCodeClient::new(chan, events_tx, launch());
        (client, events_rx, server_inbox, server_outbox)
    }

    fn parse(s: &str) -> Value {
        serde_json::from_str(s).unwrap()
    }

    #[tokio::test]
    async fn extracts_session_id_from_system_init() {
        let (mut client, _events, _server_inbox, server_outbox) = setup().await;
        server_outbox
            .send(
                r#"{"type":"system","subtype":"init","cwd":"/tmp/issue-1","session_id":"sess-42","tools":[],"mcp_servers":[],"model":"claude-opus","permissionMode":"bypassPermissions","apiKeySource":"env"}"#
                    .into(),
            )
            .unwrap();
        let thread_id = client.start_session("/tmp/issue-1").await.unwrap();
        assert_eq!(thread_id, "sess-42");
    }

    async fn handshake(
        client: &mut ClaudeCodeClient,
        server_outbox: &mpsc::UnboundedSender<String>,
    ) {
        server_outbox
            .send(
                r#"{"type":"system","subtype":"init","cwd":"/tmp/issue-1","session_id":"sess-1"}"#
                    .into(),
            )
            .unwrap();
        client.start_session("/tmp/issue-1").await.unwrap();
    }

    #[tokio::test]
    async fn run_turn_completes_on_result_success() {
        let (mut client, mut events, mut server_inbox, server_outbox) = setup().await;
        handshake(&mut client, &server_outbox).await;

        let req = TurnRequest {
            prompt: "do it".into(),
            title: "MT-1: do it".into(),
        };
        let h = tokio::spawn(async move { client.run_turn(req, "/tmp/issue-1").await });

        let user_in = parse(&server_inbox.recv().await.unwrap());
        assert_eq!(user_in["type"], "user");
        assert_eq!(user_in["message"]["content"][0]["text"], "do it");
        assert_eq!(user_in["session_id"], "sess-1");

        server_outbox
            .send(r#"{"type":"result","subtype":"success","session_id":"sess-1","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}"#.into())
            .unwrap();

        let summary = h.await.unwrap().unwrap();
        assert_eq!(summary.session_id, "sess-1-1");
        let usage = summary.thread_total_usage.unwrap();
        assert_eq!(usage.input_tokens, 100);
        assert_eq!(usage.total_tokens, 150);

        let mut kinds = Vec::new();
        while let Ok(ev) = events.try_recv() {
            kinds.push(ev.event);
        }
        assert!(kinds.contains(&"session_started".to_string()));
        assert!(kinds.contains(&"turn_completed".to_string()));
    }

    #[tokio::test]
    async fn result_error_subtype_propagates_failure() {
        let (mut client, mut events, _server_inbox, server_outbox) = setup().await;
        handshake(&mut client, &server_outbox).await;

        let req = TurnRequest {
            prompt: "p".into(),
            title: "t".into(),
        };
        let h = tokio::spawn(async move { client.run_turn(req, "/tmp/issue-1").await });

        // skip the inbound user message
        let _ = h.is_finished();
        server_outbox
            .send(r#"{"type":"result","subtype":"error_max_turns","session_id":"sess-1"}"#.into())
            .unwrap();

        let err = h.await.unwrap().unwrap_err();
        assert!(matches!(err, CodexError::TurnFailed(ref s) if s.contains("error_max_turns")));
        let mut kinds = Vec::new();
        while let Ok(ev) = events.try_recv() {
            kinds.push(ev.event);
        }
        assert!(kinds.contains(&"turn_failed".to_string()));
    }

    #[tokio::test]
    async fn dispatches_tool_use_and_replies_with_tool_result() {
        let (mut client, mut events, mut server_inbox, server_outbox) = setup().await;
        handshake(&mut client, &server_outbox).await;

        let req = TurnRequest {
            prompt: "p".into(),
            title: "t".into(),
        };
        let h = tokio::spawn(async move { client.run_turn(req, "/tmp/issue-1").await });

        let _ = server_inbox.recv().await.unwrap();
        server_outbox
            .send(
                r#"{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_42","name":"unknown","input":{}}]},"session_id":"sess-1"}"#
                    .into(),
            )
            .unwrap();
        let reply = parse(&server_inbox.recv().await.unwrap());
        assert_eq!(reply["type"], "user");
        assert_eq!(reply["message"]["content"][0]["type"], "tool_result");
        assert_eq!(reply["message"]["content"][0]["tool_use_id"], "toolu_42");
        assert_eq!(reply["message"]["content"][0]["is_error"], true);

        server_outbox
            .send(r#"{"type":"result","subtype":"success","session_id":"sess-1"}"#.into())
            .unwrap();
        h.await.unwrap().unwrap();

        let mut kinds = Vec::new();
        while let Ok(ev) = events.try_recv() {
            kinds.push(ev.event);
        }
        assert!(kinds.contains(&"tool_call_failed".to_string()));
    }

    #[tokio::test]
    async fn malformed_line_is_emitted_but_does_not_kill_loop() {
        let (mut client, mut events, mut server_inbox, server_outbox) = setup().await;
        handshake(&mut client, &server_outbox).await;

        let req = TurnRequest {
            prompt: "p".into(),
            title: "t".into(),
        };
        let h = tokio::spawn(async move { client.run_turn(req, "/tmp/issue-1").await });

        let _ = server_inbox.recv().await.unwrap();
        server_outbox.send("not-json-{}".into()).unwrap();
        server_outbox
            .send(r#"{"type":"result","subtype":"success","session_id":"sess-1"}"#.into())
            .unwrap();
        h.await.unwrap().unwrap();
        let mut kinds = Vec::new();
        while let Ok(ev) = events.try_recv() {
            kinds.push(ev.event);
        }
        assert!(kinds.contains(&"malformed".to_string()));
        assert!(kinds.contains(&"turn_completed".to_string()));
    }

    #[tokio::test]
    async fn assistant_text_surfaces_as_notification_message() {
        let (mut client, mut events, mut server_inbox, server_outbox) = setup().await;
        handshake(&mut client, &server_outbox).await;

        let req = TurnRequest {
            prompt: "p".into(),
            title: "t".into(),
        };
        let h = tokio::spawn(async move { client.run_turn(req, "/tmp/issue-1").await });

        let _ = server_inbox.recv().await.unwrap();
        server_outbox
            .send(
                r#"{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"working on it"}]},"session_id":"sess-1"}"#
                    .into(),
            )
            .unwrap();
        server_outbox
            .send(r#"{"type":"result","subtype":"success","session_id":"sess-1"}"#.into())
            .unwrap();
        h.await.unwrap().unwrap();

        let mut messages: Vec<String> = Vec::new();
        while let Ok(ev) = events.try_recv() {
            if let Some(msg) = ev.message {
                messages.push(msg);
            }
        }
        assert!(messages.iter().any(|m| m == "working on it"));
    }
}
