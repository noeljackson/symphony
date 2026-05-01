//! High-level Codex app-server client. SPEC §10.
//!
//! Drives the JSON-RPC handshake, runs turns to completion, dispatches
//! `item/tool/call` requests through a [`ToolExecutor`], and emits
//! [`RuntimeEvent`]s up to the orchestrator.
//!
//! This is intentionally a "minimum viable" subset of the Codex protocol that
//! covers the SPEC §10 orchestration responsibilities; details that vary
//! between Codex versions live in payload values that we pass through
//! verbatim.

use std::sync::Arc;
use std::time::Duration;

use serde_json::{json, Value};
use tokio::sync::mpsc;

use crate::channel::Channel;
use crate::errors::CodexError;
use crate::events::{RuntimeEvent, TokenUsage};
use crate::tools::{ToolExecutor, UnsupportedToolExecutor};

const INITIALIZE_ID: u64 = 1;
const THREAD_START_ID: u64 = 2;
const TURN_START_ID: u64 = 3;

/// SPEC §5.3.6 codex policy values. Held opaquely as `serde_json::Value` so
/// we don't have to hand-maintain enum variants for every Codex version.
#[derive(Debug, Clone)]
pub struct SessionPolicies {
    pub approval_policy: Value,
    pub thread_sandbox: Value,
    pub turn_sandbox_policy: Value,
}

impl Default for SessionPolicies {
    fn default() -> Self {
        // High-trust posture matching the Elixir reference: never ask for
        // approval, full workspace + network sandbox.
        Self {
            approval_policy: Value::String("never".into()),
            thread_sandbox: Value::String("danger-full-access".into()),
            turn_sandbox_policy: json!({"mode": "danger-full-access"}),
        }
    }
}

#[derive(Debug, Clone)]
pub struct CodexLaunch {
    pub workspace: std::path::PathBuf,
    pub policies: SessionPolicies,
    pub read_timeout: Duration,
    pub turn_timeout: Duration,
}

#[derive(Debug, Clone)]
pub struct TurnRequest {
    pub prompt: String,
    /// Used as the `title` field in `turn/start` so the Codex UI / logs can
    /// identify which issue we're working on.
    pub title: String,
}

#[derive(Debug, Clone)]
pub struct TurnSummary {
    pub thread_id: String,
    pub turn_id: String,
    pub session_id: String,
    pub thread_total_usage: Option<TokenUsage>,
}

pub struct CodexClient {
    channel: Arc<dyn Channel>,
    events: mpsc::Sender<RuntimeEvent>,
    tools: Arc<dyn ToolExecutor>,
    policies: SessionPolicies,
    read_timeout: Duration,
    turn_timeout: Duration,
    thread_id: Option<String>,
    auto_approve: bool,
    last_thread_total_usage: Option<TokenUsage>,
}

impl CodexClient {
    pub fn new(
        channel: Arc<dyn Channel>,
        events: mpsc::Sender<RuntimeEvent>,
        launch: CodexLaunch,
    ) -> Self {
        let auto_approve = is_never(&launch.policies.approval_policy);
        Self {
            channel,
            events,
            tools: Arc::new(UnsupportedToolExecutor),
            policies: launch.policies,
            read_timeout: launch.read_timeout,
            turn_timeout: launch.turn_timeout,
            thread_id: None,
            auto_approve,
            last_thread_total_usage: None,
        }
    }

    pub fn with_tools(mut self, tools: Arc<dyn ToolExecutor>) -> Self {
        self.tools = tools;
        self
    }

    /// Run the JSON-RPC handshake: `initialize` -> `initialized` notification
    /// -> `thread/start`. Returns the `thread_id` per SPEC §10.2.
    pub async fn start_session(&mut self, workspace_cwd: &str) -> Result<String, CodexError> {
        // initialize
        self.send(&json!({
            "jsonrpc": "2.0",
            "id": INITIALIZE_ID,
            "method": "initialize",
            "params": {
                "clientInfo": {
                    "name": "symphony-orchestrator",
                    "title": "Symphony Orchestrator",
                    "version": env!("CARGO_PKG_VERSION"),
                },
            }
        }))
        .await?;
        let _ = self.await_response(INITIALIZE_ID).await?;

        // initialized notification (no id)
        self.send(&json!({
            "jsonrpc": "2.0",
            "method": "initialized",
            "params": {},
        }))
        .await?;

        // thread/start
        self.send(&json!({
            "jsonrpc": "2.0",
            "id": THREAD_START_ID,
            "method": "thread/start",
            "params": {
                "approvalPolicy": self.policies.approval_policy,
                "sandbox": self.policies.thread_sandbox,
                "cwd": workspace_cwd,
                "dynamicTools": self.tools.specs(),
            }
        }))
        .await?;
        let resp = self.await_response(THREAD_START_ID).await?;
        let thread_id = resp
            .get("result")
            .and_then(|r| r.get("thread"))
            .and_then(|t| t.get("id"))
            .and_then(|v| v.as_str())
            .ok_or_else(|| CodexError::ResponseError("missing thread.id".into()))?
            .to_string();
        self.thread_id = Some(thread_id.clone());
        Ok(thread_id)
    }

    /// Start a turn and drive the streaming loop to completion. Emits one
    /// `session_started` event up front and one `turn_completed` /
    /// `turn_failed` / `turn_cancelled` at the end.
    pub async fn run_turn(
        &mut self,
        request: TurnRequest,
        workspace_cwd: &str,
    ) -> Result<TurnSummary, CodexError> {
        let thread_id = self
            .thread_id
            .clone()
            .ok_or_else(|| CodexError::ResponseError("session not started".into()))?;

        self.send(&json!({
            "jsonrpc": "2.0",
            "id": TURN_START_ID,
            "method": "turn/start",
            "params": {
                "threadId": thread_id,
                "input": [{"type": "text", "text": request.prompt}],
                "cwd": workspace_cwd,
                "title": request.title,
                "approvalPolicy": self.policies.approval_policy,
                "sandboxPolicy": self.policies.turn_sandbox_policy,
            }
        }))
        .await?;
        let resp = self.await_response(TURN_START_ID).await?;
        let turn_id = resp
            .get("result")
            .and_then(|r| r.get("turn"))
            .and_then(|t| t.get("id"))
            .and_then(|v| v.as_str())
            .ok_or_else(|| CodexError::ResponseError("missing turn.id".into()))?
            .to_string();

        let session_id = format!("{thread_id}-{turn_id}");
        let mut session_started = RuntimeEvent::new("session_started");
        session_started.session_id = Some(session_id.clone());
        session_started.thread_id = Some(thread_id.clone());
        session_started.turn_id = Some(turn_id.clone());
        let _ = self.events.send(session_started).await;

        let outcome = self.drive_turn(&session_id, &thread_id, &turn_id).await;
        match outcome {
            Ok(()) => Ok(TurnSummary {
                thread_id,
                turn_id,
                session_id,
                thread_total_usage: self.last_thread_total_usage.clone(),
            }),
            Err(e) => Err(e),
        }
    }

    /// SPEC §10.2: send the request that ends the session. We close the
    /// channel to drop the subprocess.
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
        let deadline = std::time::Instant::now() + self.turn_timeout;
        loop {
            let remaining = deadline.saturating_duration_since(std::time::Instant::now());
            if remaining.is_zero() {
                return Err(CodexError::TurnTimeout);
            }
            let read_window = remaining.min(self.read_timeout);
            let line = match self.channel.recv_line(read_window).await {
                Ok(line) => line,
                Err(CodexError::ResponseTimeout) => {
                    // fall through to the loop, which re-checks the turn deadline
                    if std::time::Instant::now() >= deadline {
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
            match self
                .handle_message(parsed, session_id, thread_id, turn_id)
                .await?
            {
                FlowControl::Continue => {}
                FlowControl::Done(reason) => return reason,
            }
        }
    }

    async fn handle_message(
        &mut self,
        msg: Value,
        session_id: &str,
        thread_id: &str,
        turn_id: &str,
    ) -> Result<FlowControl, CodexError> {
        // Method-bearing messages drive the turn lifecycle.
        if let Some(method) = msg
            .get("method")
            .and_then(|v| v.as_str())
            .map(str::to_string)
        {
            return self
                .handle_method(&method, msg, session_id, thread_id, turn_id)
                .await;
        }

        // Anything else (id-bound responses we already consumed, stray
        // notifications) is observability-only.
        let mut ev = RuntimeEvent::new("other_message");
        ev.session_id = Some(session_id.to_string());
        ev.payload = msg;
        let _ = self.events.send(ev).await;
        Ok(FlowControl::Continue)
    }

    async fn handle_method(
        &mut self,
        method: &str,
        msg: Value,
        session_id: &str,
        thread_id: &str,
        turn_id: &str,
    ) -> Result<FlowControl, CodexError> {
        match method {
            "turn/completed" => {
                self.merge_token_usage(&msg);
                let mut ev = self.event_for(method, &msg, session_id, thread_id, turn_id);
                ev.event = "turn_completed".into();
                let _ = self.events.send(ev).await;
                Ok(FlowControl::Done(Ok(())))
            }
            "turn/failed" => {
                let mut ev = self.event_for(method, &msg, session_id, thread_id, turn_id);
                ev.event = "turn_failed".into();
                ev.message = extract_message(&msg);
                let _ = self.events.send(ev).await;
                Ok(FlowControl::Done(Err(CodexError::TurnFailed(
                    ev_message_summary(&msg),
                ))))
            }
            "turn/cancelled" => {
                let mut ev = self.event_for(method, &msg, session_id, thread_id, turn_id);
                ev.event = "turn_cancelled".into();
                let _ = self.events.send(ev).await;
                Ok(FlowControl::Done(Err(CodexError::TurnCancelled)))
            }
            "thread/tokenUsage/updated" => {
                self.merge_token_usage(&msg);
                let mut ev = self.event_for(method, &msg, session_id, thread_id, turn_id);
                ev.event = "notification".into();
                let _ = self.events.send(ev).await;
                Ok(FlowControl::Continue)
            }
            "item/commandExecution/requestApproval" | "execCommandApproval" => {
                self.handle_approval(
                    method,
                    msg,
                    session_id,
                    thread_id,
                    turn_id,
                    "acceptForSession",
                )
                .await
            }
            "applyPatchApproval" => {
                self.handle_approval(
                    method,
                    msg,
                    session_id,
                    thread_id,
                    turn_id,
                    "approved_for_session",
                )
                .await
            }
            "item/tool/call" => {
                self.handle_tool_call(method, msg, session_id, thread_id, turn_id)
                    .await
            }
            other if needs_input(other) => {
                let mut ev = self.event_for(method, &msg, session_id, thread_id, turn_id);
                ev.event = "turn_input_required".into();
                let _ = self.events.send(ev).await;
                Ok(FlowControl::Done(Err(CodexError::TurnInputRequired)))
            }
            _ => {
                let mut ev = self.event_for(method, &msg, session_id, thread_id, turn_id);
                ev.event = "notification".into();
                let _ = self.events.send(ev).await;
                Ok(FlowControl::Continue)
            }
        }
    }

    async fn handle_approval(
        &mut self,
        method: &str,
        msg: Value,
        session_id: &str,
        thread_id: &str,
        turn_id: &str,
        decision: &str,
    ) -> Result<FlowControl, CodexError> {
        let id = msg.get("id").cloned();
        if !self.auto_approve {
            let mut ev = self.event_for(method, &msg, session_id, thread_id, turn_id);
            ev.event = "approval_required".into();
            let _ = self.events.send(ev).await;
            return Ok(FlowControl::Done(Err(CodexError::TurnInputRequired)));
        }
        if let Some(id) = id {
            self.send(&json!({
                "jsonrpc": "2.0",
                "id": id,
                "result": { "decision": decision },
            }))
            .await?;
        }
        let mut ev = self.event_for(method, &msg, session_id, thread_id, turn_id);
        ev.event = "approval_auto_approved".into();
        let _ = self.events.send(ev).await;
        Ok(FlowControl::Continue)
    }

    async fn handle_tool_call(
        &mut self,
        method: &str,
        msg: Value,
        session_id: &str,
        thread_id: &str,
        turn_id: &str,
    ) -> Result<FlowControl, CodexError> {
        let id = msg.get("id").cloned();
        let params = msg.get("params").cloned().unwrap_or(Value::Null);
        let name = params
            .get("name")
            .and_then(|v| v.as_str())
            .or_else(|| params.get("toolName").and_then(|v| v.as_str()))
            .unwrap_or("")
            .to_string();
        let arguments = params
            .get("arguments")
            .cloned()
            .or_else(|| params.get("args").cloned())
            .unwrap_or(Value::Null);

        let result = if name.is_empty() {
            crate::tools::ToolResult::failure("missing tool name")
        } else {
            self.tools.execute(&name, &arguments).await
        };

        if let Some(id) = id {
            self.send(&json!({
                "jsonrpc": "2.0",
                "id": id,
                "result": result,
            }))
            .await?;
        }

        let mut ev = self.event_for(method, &msg, session_id, thread_id, turn_id);
        ev.event = if result.success {
            "tool_call_completed".into()
        } else if name.is_empty() {
            "unsupported_tool_call".into()
        } else {
            "tool_call_failed".into()
        };
        let _ = self.events.send(ev).await;
        Ok(FlowControl::Continue)
    }

    fn event_for(
        &self,
        method: &str,
        msg: &Value,
        session_id: &str,
        thread_id: &str,
        turn_id: &str,
    ) -> RuntimeEvent {
        let mut ev = RuntimeEvent::new(method);
        ev.session_id = Some(session_id.to_string());
        ev.thread_id = Some(thread_id.to_string());
        ev.turn_id = Some(turn_id.to_string());
        ev.thread_total_usage = self.last_thread_total_usage.clone();
        ev.payload = msg.clone();
        ev.message = extract_message(msg);
        ev
    }

    fn merge_token_usage(&mut self, msg: &Value) {
        // Prefer absolute thread totals per SPEC §13.5.
        let usage = msg
            .pointer("/params/total_token_usage")
            .or_else(|| msg.pointer("/params/totalTokenUsage"))
            .or_else(|| msg.pointer("/params/usage"))
            .or_else(|| msg.pointer("/params"));
        if let Some(usage_value) = usage {
            if let Some(t) = parse_usage(usage_value) {
                self.last_thread_total_usage = Some(t);
            }
        }
    }

    async fn send(&self, payload: &Value) -> Result<(), CodexError> {
        let line = serde_json::to_string(payload)
            .map_err(|e| CodexError::ResponseError(format!("encode: {e}")))?;
        self.channel.send_line(&line).await
    }

    async fn await_response(&self, id: u64) -> Result<Value, CodexError> {
        let deadline = std::time::Instant::now() + self.read_timeout;
        loop {
            let remaining = deadline.saturating_duration_since(std::time::Instant::now());
            if remaining.is_zero() {
                return Err(CodexError::ResponseTimeout);
            }
            let line = self.channel.recv_line(remaining).await?;
            let trimmed = line.trim();
            if trimmed.is_empty() {
                continue;
            }
            let value: Value = match serde_json::from_str(trimmed) {
                Ok(v) => v,
                Err(_) => continue,
            };
            if value.get("id").and_then(|v| v.as_u64()) == Some(id) {
                return Ok(value);
            }
            // Pre-handshake notifications are ignored; we'll re-emit them
            // once `drive_turn` is running.
        }
    }
}

enum FlowControl {
    Continue,
    Done(Result<(), CodexError>),
}

fn is_never(approval: &Value) -> bool {
    matches!(approval.as_str(), Some("never"))
}

fn needs_input(method: &str) -> bool {
    matches!(method, "turn/inputRequired" | "input_required")
}

fn extract_message(msg: &Value) -> Option<String> {
    msg.get("params")
        .and_then(|p| p.get("message"))
        .and_then(|v| v.as_str())
        .map(str::to_string)
}

fn ev_message_summary(msg: &Value) -> String {
    extract_message(msg).unwrap_or_else(|| "turn failed".into())
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::channel::MemoryChannel;
    use std::path::PathBuf;
    use tokio::sync::mpsc::UnboundedReceiver;

    fn launch() -> CodexLaunch {
        CodexLaunch {
            workspace: PathBuf::from("/tmp/issue-1"),
            policies: SessionPolicies::default(),
            read_timeout: Duration::from_millis(500),
            turn_timeout: Duration::from_secs(2),
        }
    }

    async fn setup() -> (
        CodexClient,
        mpsc::Receiver<RuntimeEvent>,
        UnboundedReceiver<String>,
        mpsc::UnboundedSender<String>,
    ) {
        let (chan, server_inbox, server_outbox) = MemoryChannel::pair();
        let (events_tx, events_rx) = mpsc::channel(64);
        let client = CodexClient::new(chan, events_tx, launch());
        (client, events_rx, server_inbox, server_outbox)
    }

    fn parse(s: &str) -> Value {
        serde_json::from_str(s).unwrap()
    }

    #[tokio::test]
    async fn handshake_then_starts_session() {
        let (mut client, _events, mut server_inbox, server_outbox) = setup().await;

        let h = tokio::spawn(async move { client.start_session("/tmp/issue-1").await });

        // initialize
        let init = parse(&server_inbox.recv().await.unwrap());
        assert_eq!(init["method"], "initialize");
        assert_eq!(init["id"], 1);
        server_outbox
            .send(r#"{"jsonrpc":"2.0","id":1,"result":{}}"#.into())
            .unwrap();
        // initialized notification
        let initialized = parse(&server_inbox.recv().await.unwrap());
        assert_eq!(initialized["method"], "initialized");
        assert!(initialized.get("id").is_none());
        // thread/start
        let thread_start = parse(&server_inbox.recv().await.unwrap());
        assert_eq!(thread_start["method"], "thread/start");
        assert_eq!(thread_start["params"]["cwd"], "/tmp/issue-1");
        assert_eq!(thread_start["params"]["approvalPolicy"], "never");
        server_outbox
            .send(r#"{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr-1"}}}"#.into())
            .unwrap();

        let thread_id = h.await.unwrap().unwrap();
        assert_eq!(thread_id, "thr-1");
    }

    async fn handshake(
        client: &mut CodexClient,
        server_inbox: &mut UnboundedReceiver<String>,
        server_outbox: &mpsc::UnboundedSender<String>,
    ) {
        let h = async {
            // initialize
            let _ = server_inbox.recv().await.unwrap();
            server_outbox
                .send(r#"{"jsonrpc":"2.0","id":1,"result":{}}"#.into())
                .unwrap();
            // initialized
            let _ = server_inbox.recv().await.unwrap();
            // thread/start
            let _ = server_inbox.recv().await.unwrap();
            server_outbox
                .send(r#"{"jsonrpc":"2.0","id":2,"result":{"thread":{"id":"thr-1"}}}"#.into())
                .unwrap();
        };
        let (_, r) = tokio::join!(h, client.start_session("/tmp/issue-1"));
        r.unwrap();
    }

    #[tokio::test]
    async fn run_turn_completes_on_turn_completed() {
        let (mut client, mut events, mut server_inbox, server_outbox) = setup().await;
        handshake(&mut client, &mut server_inbox, &server_outbox).await;

        let req = TurnRequest {
            prompt: "do it".into(),
            title: "MT-1: do it".into(),
        };
        let h = tokio::spawn(async move {
            let summary = client.run_turn(req, "/tmp/issue-1").await.unwrap();
            (client, summary)
        });

        // turn/start
        let turn_start = parse(&server_inbox.recv().await.unwrap());
        assert_eq!(turn_start["method"], "turn/start");
        assert_eq!(turn_start["params"]["threadId"], "thr-1");
        server_outbox
            .send(r#"{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"trn-1"}}}"#.into())
            .unwrap();
        // turn/completed
        server_outbox
            .send(r#"{"jsonrpc":"2.0","method":"turn/completed","params":{}}"#.into())
            .unwrap();

        let (_client, summary) = h.await.unwrap();
        assert_eq!(summary.session_id, "thr-1-trn-1");

        // Drain events: session_started + turn_completed.
        let mut kinds = Vec::new();
        while let Ok(ev) = events.try_recv() {
            kinds.push(ev.event);
        }
        assert!(kinds.contains(&"session_started".to_string()));
        assert!(kinds.contains(&"turn_completed".to_string()));
    }

    #[tokio::test]
    async fn auto_approves_command_execution_request() {
        let (mut client, mut events, mut server_inbox, server_outbox) = setup().await;
        handshake(&mut client, &mut server_inbox, &server_outbox).await;

        let req = TurnRequest {
            prompt: "p".into(),
            title: "t".into(),
        };
        let h = tokio::spawn(async move { client.run_turn(req, "/tmp/issue-1").await });

        let _ = server_inbox.recv().await.unwrap();
        server_outbox
            .send(r#"{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"t1"}}}"#.into())
            .unwrap();
        server_outbox
            .send(r#"{"jsonrpc":"2.0","method":"item/commandExecution/requestApproval","id":42,"params":{}}"#.into())
            .unwrap();
        // Expect an auto-approval reply on the inbox.
        let approval_reply = parse(&server_inbox.recv().await.unwrap());
        assert_eq!(approval_reply["id"], 42);
        assert_eq!(approval_reply["result"]["decision"], "acceptForSession");
        // Then complete the turn.
        server_outbox
            .send(r#"{"jsonrpc":"2.0","method":"turn/completed","params":{}}"#.into())
            .unwrap();

        h.await.unwrap().unwrap();
        let mut events_seen = Vec::new();
        while let Ok(ev) = events.try_recv() {
            events_seen.push(ev.event);
        }
        assert!(events_seen.contains(&"approval_auto_approved".to_string()));
    }

    #[tokio::test]
    async fn unsupported_tool_call_replies_with_failure() {
        let (mut client, mut events, mut server_inbox, server_outbox) = setup().await;
        handshake(&mut client, &mut server_inbox, &server_outbox).await;

        let req = TurnRequest {
            prompt: "p".into(),
            title: "t".into(),
        };
        let h = tokio::spawn(async move { client.run_turn(req, "/tmp/issue-1").await });

        let _ = server_inbox.recv().await.unwrap();
        server_outbox
            .send(r#"{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"t1"}}}"#.into())
            .unwrap();
        server_outbox
            .send(
                r#"{"jsonrpc":"2.0","method":"item/tool/call","id":7,"params":{"name":"unknown","arguments":{}}}"#
                    .into(),
            )
            .unwrap();
        let reply = parse(&server_inbox.recv().await.unwrap());
        assert_eq!(reply["id"], 7);
        assert_eq!(reply["result"]["success"], false);
        assert!(reply["result"]["output"]
            .as_str()
            .unwrap()
            .contains("unsupported tool"));

        server_outbox
            .send(r#"{"jsonrpc":"2.0","method":"turn/completed","params":{}}"#.into())
            .unwrap();
        h.await.unwrap().unwrap();

        let mut events_seen = Vec::new();
        while let Ok(ev) = events.try_recv() {
            events_seen.push(ev.event);
        }
        assert!(events_seen.contains(&"tool_call_failed".to_string()));
    }

    #[tokio::test]
    async fn turn_failed_propagates_typed_error_and_emits_event() {
        let (mut client, mut events, mut server_inbox, server_outbox) = setup().await;
        handshake(&mut client, &mut server_inbox, &server_outbox).await;

        let req = TurnRequest {
            prompt: "p".into(),
            title: "t".into(),
        };
        let h = tokio::spawn(async move { client.run_turn(req, "/tmp/issue-1").await });

        let _ = server_inbox.recv().await.unwrap();
        server_outbox
            .send(r#"{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"t1"}}}"#.into())
            .unwrap();
        server_outbox
            .send(r#"{"jsonrpc":"2.0","method":"turn/failed","params":{"message":"oops"}}"#.into())
            .unwrap();

        let err = h.await.unwrap().unwrap_err();
        assert!(matches!(err, CodexError::TurnFailed(ref s) if s.contains("oops")));
        let mut events_seen = Vec::new();
        while let Ok(ev) = events.try_recv() {
            events_seen.push(ev.event);
        }
        assert!(events_seen.contains(&"turn_failed".to_string()));
    }

    #[tokio::test]
    async fn token_usage_is_extracted_from_thread_event() {
        let (mut client, _events, mut server_inbox, server_outbox) = setup().await;
        handshake(&mut client, &mut server_inbox, &server_outbox).await;

        let req = TurnRequest {
            prompt: "p".into(),
            title: "t".into(),
        };
        let h = tokio::spawn(async move {
            let s = client.run_turn(req, "/tmp/issue-1").await.unwrap();
            s
        });

        let _ = server_inbox.recv().await.unwrap();
        server_outbox
            .send(r#"{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"t1"}}}"#.into())
            .unwrap();
        server_outbox
            .send(
                r#"{"jsonrpc":"2.0","method":"thread/tokenUsage/updated","params":{"input_tokens":100,"output_tokens":50,"total_tokens":150}}"#
                    .into(),
            )
            .unwrap();
        server_outbox
            .send(r#"{"jsonrpc":"2.0","method":"turn/completed","params":{}}"#.into())
            .unwrap();

        let summary = h.await.unwrap();
        let usage = summary.thread_total_usage.unwrap();
        assert_eq!(usage.input_tokens, 100);
        assert_eq!(usage.output_tokens, 50);
        assert_eq!(usage.total_tokens, 150);
    }

    #[tokio::test]
    async fn malformed_line_is_emitted_but_does_not_kill_loop() {
        let (mut client, mut events, mut server_inbox, server_outbox) = setup().await;
        handshake(&mut client, &mut server_inbox, &server_outbox).await;

        let req = TurnRequest {
            prompt: "p".into(),
            title: "t".into(),
        };
        let h = tokio::spawn(async move { client.run_turn(req, "/tmp/issue-1").await });

        let _ = server_inbox.recv().await.unwrap();
        server_outbox
            .send(r#"{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"t1"}}}"#.into())
            .unwrap();
        server_outbox.send("not-json-{}".into()).unwrap();
        server_outbox
            .send(r#"{"jsonrpc":"2.0","method":"turn/completed","params":{}}"#.into())
            .unwrap();

        h.await.unwrap().unwrap();
        let mut events_seen = Vec::new();
        while let Ok(ev) = events.try_recv() {
            events_seen.push(ev.event);
        }
        assert!(events_seen.contains(&"malformed".to_string()));
        assert!(events_seen.contains(&"turn_completed".to_string()));
    }

    #[tokio::test]
    async fn non_never_approval_policy_disables_auto_approve() {
        let (chan, mut server_inbox, server_outbox) = MemoryChannel::pair();
        let (events_tx, mut events_rx) = mpsc::channel(64);
        let mut launch = launch();
        launch.policies.approval_policy = Value::String("on-request".into());
        let mut client = CodexClient::new(chan, events_tx, launch);
        handshake(&mut client, &mut server_inbox, &server_outbox).await;

        let req = TurnRequest {
            prompt: "p".into(),
            title: "t".into(),
        };
        let h = tokio::spawn(async move { client.run_turn(req, "/tmp/issue-1").await });

        let _ = server_inbox.recv().await.unwrap();
        server_outbox
            .send(r#"{"jsonrpc":"2.0","id":3,"result":{"turn":{"id":"t1"}}}"#.into())
            .unwrap();
        server_outbox
            .send(
                r#"{"jsonrpc":"2.0","method":"item/commandExecution/requestApproval","id":4,"params":{}}"#
                    .into(),
            )
            .unwrap();

        let err = h.await.unwrap().unwrap_err();
        assert!(matches!(err, CodexError::TurnInputRequired));
        let mut events_seen = Vec::new();
        while let Ok(ev) = events_rx.try_recv() {
            events_seen.push(ev.event);
        }
        assert!(events_seen.contains(&"approval_required".to_string()));
    }
}
