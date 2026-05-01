//! Line-delimited JSON channel abstraction.
//!
//! Real sessions wire this up to a child process's stdin / stdout. Tests use
//! the in-memory variant to drive the protocol deterministically.

use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::{Child, ChildStdin, ChildStdout};
use tokio::sync::{mpsc, Mutex};
use tokio::time::timeout;

use crate::errors::CodexError;

/// SPEC §10.1: 10 MB max line size for safe buffering.
pub const MAX_LINE_BYTES: usize = 10 * 1024 * 1024;

#[async_trait]
pub trait Channel: Send + Sync {
    async fn send_line(&self, line: &str) -> Result<(), CodexError>;
    async fn recv_line(&self, deadline: Duration) -> Result<String, CodexError>;
    async fn close(&self);
}

/// Tokio-process-backed channel. Owns the child handle so dropping the channel
/// also kills the subprocess.
pub struct ChildChannel {
    inner: Mutex<ChildIo>,
}

struct ChildIo {
    child: Child,
    stdin: Option<ChildStdin>,
    stdout: Option<BufReader<ChildStdout>>,
}

impl ChildChannel {
    pub fn new(mut child: Child) -> Result<Arc<Self>, CodexError> {
        let stdin = child
            .stdin
            .take()
            .ok_or_else(|| CodexError::PortExit("missing stdin".into()))?;
        let stdout = child
            .stdout
            .take()
            .map(BufReader::new)
            .ok_or_else(|| CodexError::PortExit("missing stdout".into()))?;
        Ok(Arc::new(ChildChannel {
            inner: Mutex::new(ChildIo {
                child,
                stdin: Some(stdin),
                stdout: Some(stdout),
            }),
        }))
    }
}

#[async_trait]
impl Channel for ChildChannel {
    async fn send_line(&self, line: &str) -> Result<(), CodexError> {
        let mut guard = self.inner.lock().await;
        let stdin = guard
            .stdin
            .as_mut()
            .ok_or_else(|| CodexError::PortExit("stdin closed".into()))?;
        stdin
            .write_all(line.as_bytes())
            .await
            .map_err(|e| CodexError::PortExit(e.to_string()))?;
        stdin
            .write_all(b"\n")
            .await
            .map_err(|e| CodexError::PortExit(e.to_string()))?;
        stdin
            .flush()
            .await
            .map_err(|e| CodexError::PortExit(e.to_string()))?;
        Ok(())
    }

    async fn recv_line(&self, deadline: Duration) -> Result<String, CodexError> {
        let mut guard = self.inner.lock().await;
        let stdout = guard
            .stdout
            .as_mut()
            .ok_or_else(|| CodexError::PortExit("stdout closed".into()))?;
        let mut buf = String::new();
        let read = timeout(deadline, stdout.read_line(&mut buf))
            .await
            .map_err(|_| CodexError::ResponseTimeout)?
            .map_err(|e| CodexError::PortExit(e.to_string()))?;
        if read == 0 {
            return Err(CodexError::PortExit("stdout closed".into()));
        }
        if buf.len() > MAX_LINE_BYTES {
            return Err(CodexError::ResponseError(format!(
                "line exceeds {} bytes",
                MAX_LINE_BYTES
            )));
        }
        // Strip trailing newline characters.
        while matches!(buf.chars().last(), Some('\n') | Some('\r')) {
            buf.pop();
        }
        Ok(buf)
    }

    async fn close(&self) {
        let mut guard = self.inner.lock().await;
        // Drop stdin so the child sees EOF, then attempt a graceful wait.
        guard.stdin.take();
        let _ = guard.child.start_kill();
        let _ = guard.child.wait().await;
    }
}

/// In-memory channel for unit tests. The "client side" sends lines to one
/// queue and reads from another; a fake server can be driven from the
/// opposite ends.
pub struct MemoryChannel {
    inbound: Mutex<mpsc::UnboundedReceiver<String>>,
    outbound: mpsc::UnboundedSender<String>,
}

impl MemoryChannel {
    /// Returns `(client_channel, server_inbox, server_outbox)` where:
    /// * `client_channel` is what the [`CodexClient`] uses;
    /// * `server_inbox` receives lines the client sends;
    /// * `server_outbox` injects lines that will be returned from `recv_line`.
    pub fn pair() -> (
        Arc<MemoryChannel>,
        mpsc::UnboundedReceiver<String>,
        mpsc::UnboundedSender<String>,
    ) {
        let (client_to_server_tx, client_to_server_rx) = mpsc::unbounded_channel();
        let (server_to_client_tx, server_to_client_rx) = mpsc::unbounded_channel();
        let chan = Arc::new(MemoryChannel {
            inbound: Mutex::new(server_to_client_rx),
            outbound: client_to_server_tx,
        });
        (chan, client_to_server_rx, server_to_client_tx)
    }
}

#[async_trait]
impl Channel for MemoryChannel {
    async fn send_line(&self, line: &str) -> Result<(), CodexError> {
        self.outbound
            .send(line.to_string())
            .map_err(|_| CodexError::PortExit("memory channel closed".into()))
    }

    async fn recv_line(&self, deadline: Duration) -> Result<String, CodexError> {
        let mut guard = self.inbound.lock().await;
        match timeout(deadline, guard.recv()).await {
            Ok(Some(line)) => Ok(line),
            Ok(None) => Err(CodexError::PortExit("memory channel closed".into())),
            Err(_) => Err(CodexError::ResponseTimeout),
        }
    }

    async fn close(&self) {}
}
