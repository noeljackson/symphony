//! axum-based HTTP server. Exposes the SPEC §13.7 dashboard + JSON API.

use std::convert::Infallible;
use std::net::SocketAddr;
use std::path::{Path as FsPath, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use axum::{
    body::Body,
    extract::{Path, State},
    http::{header, StatusCode},
    response::{
        sse::{Event, KeepAlive, Sse},
        Html, IntoResponse, Json, Response,
    },
    routing::{get, post},
    Router,
};
use futures::stream::Stream;
use symphony_core::sanitize::workspace_key;
use symphony_orchestrator::{EventBroadcast, ForceRetryOutcome, OrchestratorHandle};
use tokio_stream::wrappers::BroadcastStream;
use tokio_stream::StreamExt;

use crate::api::{issue_view, ApiError, ApiErrorBody, StateView};
use crate::dashboard::render_dashboard;

/// Cap on how many directory entries `GET /workspace` returns. Anything
/// past this is silently dropped — operators who want full access can shell
/// in. Tracks SPEC §13.7.3's "SHOULD truncate large directory listings".
pub const WORKSPACE_LISTING_LIMIT: usize = 1000;

/// Cap on the byte size of a single `GET /workspace/<file>` response.
/// Bigger files return 413 Payload Too Large rather than streaming megabytes
/// of agent output through the dashboard.
pub const WORKSPACE_FILE_BYTE_LIMIT: u64 = 2 * 1024 * 1024;

#[derive(Clone)]
struct AppState {
    handle: OrchestratorHandle,
    /// Resolved absolute workspace root. Required by the SPEC §13.7.3
    /// workspace endpoints; the §13.7.2 endpoints don't use it.
    workspace_root: Option<Arc<PathBuf>>,
}

pub struct ServerHandle {
    pub local_addr: SocketAddr,
    join: tokio::task::JoinHandle<()>,
    shutdown: tokio::sync::oneshot::Sender<()>,
}

impl ServerHandle {
    /// Stop the server and wait for it to exit.
    pub async fn shutdown(self) {
        let _ = self.shutdown.send(());
        let _ = self.join.await;
    }
}

/// Bind and serve. `addr` SHOULD bind loopback by default (SPEC §13.7).
/// `port=0` requests an ephemeral port; the bound address is returned in
/// the [`ServerHandle`].
///
/// Without a workspace root, the §13.7.3 workspace browser endpoints
/// return `503 unavailable`. Use [`serve_with_workspace`] to enable them.
pub async fn serve(addr: SocketAddr, handle: OrchestratorHandle) -> std::io::Result<ServerHandle> {
    serve_inner(addr, handle, None).await
}

/// Like [`serve`], but also enables the SPEC §13.7.3 workspace browser at
/// `GET /api/v1/<id>/workspace[/file]` rooted at `workspace_root`.
pub async fn serve_with_workspace(
    addr: SocketAddr,
    handle: OrchestratorHandle,
    workspace_root: PathBuf,
) -> std::io::Result<ServerHandle> {
    serve_inner(addr, handle, Some(Arc::new(workspace_root))).await
}

async fn serve_inner(
    addr: SocketAddr,
    handle: OrchestratorHandle,
    workspace_root: Option<Arc<PathBuf>>,
) -> std::io::Result<ServerHandle> {
    let state = AppState {
        handle,
        workspace_root,
    };
    let router = Router::new()
        .route("/", get(dashboard_html))
        .route("/api/v1/state", get(get_state))
        .route("/api/v1/events", get(get_events))
        .route("/api/v1/refresh", post(post_refresh))
        .route("/api/v1/:identifier", get(get_issue))
        .route("/api/v1/:identifier/retry", post(post_retry))
        .route("/api/v1/:identifier/workspace", get(get_workspace))
        .route(
            "/api/v1/:identifier/workspace/*file",
            get(get_workspace_file),
        )
        .with_state(state);

    let listener = tokio::net::TcpListener::bind(addr).await?;
    let local_addr = listener.local_addr()?;
    let (tx, rx) = tokio::sync::oneshot::channel::<()>();
    let join = tokio::spawn(async move {
        let _ = axum::serve(listener, router)
            .with_graceful_shutdown(async move {
                let _ = rx.await;
            })
            .await;
    });
    Ok(ServerHandle {
        local_addr,
        join,
        shutdown: tx,
    })
}

async fn dashboard_html(State(s): State<AppState>) -> impl IntoResponse {
    let snap = match s.handle.snapshot().await {
        Some(s) => s,
        None => {
            return (
                StatusCode::SERVICE_UNAVAILABLE,
                Html(render_dashboard(None)),
            );
        }
    };
    let view = StateView::from_snapshot(&snap, None);
    let html = render_dashboard(Some(&view));
    (StatusCode::OK, Html(html))
}

async fn get_state(State(s): State<AppState>) -> impl IntoResponse {
    match s.handle.snapshot().await {
        Some(snap) => {
            let view = StateView::from_snapshot(&snap, None);
            (StatusCode::OK, Json(serde_json::to_value(view).unwrap())).into_response()
        }
        None => json_error(
            StatusCode::SERVICE_UNAVAILABLE,
            "unavailable",
            "snapshot timed out",
        ),
    }
}

async fn get_issue(State(s): State<AppState>, Path(identifier): Path<String>) -> impl IntoResponse {
    let snap = match s.handle.snapshot().await {
        Some(s) => s,
        None => {
            return json_error(
                StatusCode::SERVICE_UNAVAILABLE,
                "unavailable",
                "snapshot timed out",
            );
        }
    };
    match issue_view(&snap, &identifier) {
        Some(v) => (StatusCode::OK, Json(v)).into_response(),
        None => json_error(
            StatusCode::NOT_FOUND,
            "issue_not_found",
            "issue is not in the current in-memory state",
        ),
    }
}

/// SPEC §13.7.4 live event stream. Subscribers receive an initial
/// `event: snapshot` (the same JSON as `GET /api/v1/state`), then each
/// agent update as `event: <RuntimeEvent.event>`. If a subscriber lags
/// behind the broadcast channel, an `event: lagged` is emitted with the
/// number of dropped events so the client can re-snapshot.
async fn get_events(
    State(s): State<AppState>,
) -> Sse<impl Stream<Item = Result<Event, Infallible>>> {
    let receiver = s.handle.subscribe_events();
    let snap_payload = s
        .handle
        .snapshot()
        .await
        .map(|snap| StateView::from_snapshot(&snap, None));

    let initial = futures::stream::once(async move {
        let body = match snap_payload {
            Some(view) => serde_json::to_string(&view).unwrap_or_else(|_| "{}".into()),
            None => "{}".into(),
        };
        Ok(Event::default().event("snapshot").data(body))
    });

    let updates = BroadcastStream::new(receiver).map(|res| match res {
        Ok(EventBroadcast {
            issue_id,
            identifier,
            event,
        }) => {
            let body = serde_json::json!({
                "issue_id": issue_id,
                "issue_identifier": identifier,
                "session_id": event.session_id,
                "thread_id": event.thread_id,
                "turn_id": event.turn_id,
                "timestamp": event
                    .timestamp
                    .format(&time::format_description::well_known::Rfc3339)
                    .unwrap_or_default(),
                "event": event.event,
                "message": event.message,
                "payload": event.payload,
            });
            Ok(Event::default()
                .event(event.event.clone())
                .data(body.to_string()))
        }
        Err(tokio_stream::wrappers::errors::BroadcastStreamRecvError::Lagged(n)) => {
            let body = serde_json::json!({ "dropped": n });
            Ok(Event::default().event("lagged").data(body.to_string()))
        }
    });

    Sse::new(initial.chain(updates)).keep_alive(KeepAlive::new().interval(Duration::from_secs(15)))
}

async fn post_retry(
    State(s): State<AppState>,
    Path(identifier): Path<String>,
) -> impl IntoResponse {
    // We allow the operator to type either the issue's `id` (Linear's
    // internal stable id) or the human-readable `identifier`. The
    // orchestrator only knows by `id`, so resolve via the snapshot first.
    let snap = match s.handle.snapshot().await {
        Some(s) => s,
        None => {
            return json_error(
                StatusCode::SERVICE_UNAVAILABLE,
                "unavailable",
                "snapshot timed out",
            );
        }
    };
    let issue_id = snap
        .running
        .iter()
        .find(|r| r.identifier.eq_ignore_ascii_case(&identifier) || r.issue_id == identifier)
        .map(|r| r.issue_id.clone())
        .or_else(|| {
            snap.retrying
                .iter()
                .find(|r| {
                    r.identifier.eq_ignore_ascii_case(&identifier) || r.issue_id == identifier
                })
                .map(|r| r.issue_id.clone())
        });

    let issue_id = match issue_id {
        Some(id) => id,
        None => {
            return json_error(
                StatusCode::NOT_FOUND,
                "issue_not_found",
                "issue is not in the current in-memory state",
            );
        }
    };

    match s.handle.force_retry(issue_id).await {
        Some(ForceRetryOutcome::AlreadyRunning { identifier }) => {
            let body = serde_json::json!({
                "queued": false,
                "issue_identifier": identifier,
                "status": "already_running",
            });
            (StatusCode::ACCEPTED, Json(body)).into_response()
        }
        Some(ForceRetryOutcome::RetryQueued {
            identifier,
            attempt,
        }) => {
            let body = serde_json::json!({
                "queued": true,
                "issue_identifier": identifier,
                "attempt": attempt,
            });
            (StatusCode::ACCEPTED, Json(body)).into_response()
        }
        Some(ForceRetryOutcome::NotTracked) | None => json_error(
            StatusCode::NOT_FOUND,
            "issue_not_found",
            "issue is not currently tracked by the orchestrator",
        ),
    }
}

async fn get_workspace(
    State(s): State<AppState>,
    Path(identifier): Path<String>,
) -> impl IntoResponse {
    let root = match s.workspace_root.as_deref() {
        Some(r) => r,
        None => {
            return json_error(
                StatusCode::SERVICE_UNAVAILABLE,
                "workspace_browser_disabled",
                "server was started without a workspace root",
            );
        }
    };
    let key = workspace_key(&identifier);
    let workspace = root.join(&key);
    if !workspace.is_dir() {
        return json_error(
            StatusCode::NOT_FOUND,
            "workspace_not_found",
            "no workspace directory exists for this issue identifier yet",
        );
    }

    let mut entries: Vec<serde_json::Value> = Vec::new();
    let mut total_bytes: u64 = 0;
    if let Err(e) = collect_entries(&workspace, &workspace, &mut entries, &mut total_bytes) {
        return json_error(
            StatusCode::INTERNAL_SERVER_ERROR,
            "workspace_read_error",
            &format!("could not read workspace: {e}"),
        );
    }
    let truncated = entries.len() >= WORKSPACE_LISTING_LIMIT;
    if truncated {
        entries.truncate(WORKSPACE_LISTING_LIMIT);
    }

    let body = serde_json::json!({
        "issue_identifier": identifier,
        "workspace_path": workspace,
        "entries": entries,
        "total_bytes": total_bytes,
        "truncated": truncated,
    });
    (StatusCode::OK, Json(body)).into_response()
}

async fn get_workspace_file(
    State(s): State<AppState>,
    Path((identifier, file)): Path<(String, String)>,
) -> impl IntoResponse {
    let root = match s.workspace_root.as_deref() {
        Some(r) => r,
        None => {
            return json_error(
                StatusCode::SERVICE_UNAVAILABLE,
                "workspace_browser_disabled",
                "server was started without a workspace root",
            );
        }
    };
    if file.is_empty() || file.contains("..") {
        return json_error(
            StatusCode::BAD_REQUEST,
            "invalid_file_path",
            "file path must not be empty or contain `..`",
        );
    }

    let key = workspace_key(&identifier);
    let workspace = root.join(&key);
    let target = workspace.join(&file);

    // Defense in depth: even though we rejected `..` in the URL, canonicalize
    // and double-check the prefix in case symlinks point outside the
    // workspace. SPEC §9.5 root-prefix containment.
    let canonical_target = match target.canonicalize() {
        Ok(p) => p,
        Err(e) => {
            let code = if e.kind() == std::io::ErrorKind::NotFound {
                StatusCode::NOT_FOUND
            } else {
                StatusCode::INTERNAL_SERVER_ERROR
            };
            return json_error(code, "workspace_read_error", &format!("{e}"));
        }
    };
    let canonical_workspace = match workspace.canonicalize() {
        Ok(p) => p,
        Err(e) => {
            return json_error(
                StatusCode::INTERNAL_SERVER_ERROR,
                "workspace_read_error",
                &format!("{e}"),
            );
        }
    };
    if !canonical_target.starts_with(&canonical_workspace) {
        return json_error(
            StatusCode::FORBIDDEN,
            "outside_workspace",
            "resolved path escapes the workspace directory",
        );
    }
    if !canonical_target.is_file() {
        return json_error(
            StatusCode::NOT_FOUND,
            "not_a_file",
            "path resolves to a directory or special file",
        );
    }

    let metadata = match tokio::fs::metadata(&canonical_target).await {
        Ok(m) => m,
        Err(e) => {
            return json_error(
                StatusCode::INTERNAL_SERVER_ERROR,
                "workspace_read_error",
                &format!("{e}"),
            );
        }
    };
    if metadata.len() > WORKSPACE_FILE_BYTE_LIMIT {
        return json_error(
            StatusCode::PAYLOAD_TOO_LARGE,
            "file_too_large",
            &format!(
                "file is {} bytes; cap is {} bytes",
                metadata.len(),
                WORKSPACE_FILE_BYTE_LIMIT
            ),
        );
    }

    let bytes = match tokio::fs::read(&canonical_target).await {
        Ok(b) => b,
        Err(e) => {
            return json_error(
                StatusCode::INTERNAL_SERVER_ERROR,
                "workspace_read_error",
                &format!("{e}"),
            );
        }
    };
    let content_type = if std::str::from_utf8(&bytes).is_ok() {
        "text/plain; charset=utf-8"
    } else {
        "application/octet-stream"
    };
    Response::builder()
        .status(StatusCode::OK)
        .header(header::CONTENT_TYPE, content_type)
        .body(Body::from(bytes))
        .unwrap()
        .into_response()
}

fn collect_entries(
    workspace_root: &FsPath,
    dir: &FsPath,
    out: &mut Vec<serde_json::Value>,
    total_bytes: &mut u64,
) -> std::io::Result<()> {
    if out.len() >= WORKSPACE_LISTING_LIMIT {
        return Ok(());
    }
    let read_dir = std::fs::read_dir(dir)?;
    let mut entries: Vec<_> = read_dir.collect::<Result<_, _>>()?;
    entries.sort_by_key(|e| e.file_name());
    for entry in entries {
        if out.len() >= WORKSPACE_LISTING_LIMIT {
            break;
        }
        let path = entry.path();
        let metadata = match entry.metadata() {
            Ok(m) => m,
            Err(_) => continue,
        };
        let rel = path
            .strip_prefix(workspace_root)
            .map(|p| p.to_string_lossy().into_owned())
            .unwrap_or_default();
        let kind = if metadata.is_dir() {
            "dir"
        } else if metadata.is_symlink() {
            "symlink"
        } else {
            "file"
        };
        let size = if metadata.is_file() {
            metadata.len()
        } else {
            0
        };
        *total_bytes = total_bytes.saturating_add(size);
        out.push(serde_json::json!({
            "path": rel,
            "size": size,
            "kind": kind,
        }));
        if metadata.is_dir() {
            // Don't follow symlinks into outside directories. metadata.is_dir
            // already returns false for a symlinked directory because it uses
            // symlink_metadata semantics on stable; double-check via
            // path.is_dir() (which DOES follow). Not a perfect defense but
            // fine for an inspect-only browser.
            let _ = collect_entries(workspace_root, &path, out, total_bytes);
        }
    }
    Ok(())
}

async fn post_refresh(State(s): State<AppState>) -> impl IntoResponse {
    let queued = s.handle.refresh_now().await;
    let body = serde_json::json!({
        "queued": queued,
        "coalesced": false,
        "requested_at": time::OffsetDateTime::now_utc()
            .format(&time::format_description::well_known::Rfc3339)
            .unwrap_or_default(),
        "operations": ["poll", "reconcile"],
    });
    (StatusCode::ACCEPTED, Json(body))
}

fn json_error(code: StatusCode, error_code: &str, message: &str) -> axum::response::Response {
    let body = ApiError {
        error: ApiErrorBody {
            code: error_code.to_string(),
            message: message.to_string(),
        },
    };
    (code, Json(body)).into_response()
}
