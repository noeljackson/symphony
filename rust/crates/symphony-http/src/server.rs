//! axum-based HTTP server. Exposes the SPEC §13.7 dashboard + JSON API.

use std::net::SocketAddr;

use axum::{
    extract::{Path, State},
    http::StatusCode,
    response::{Html, IntoResponse, Json},
    routing::{get, post},
    Router,
};
use symphony_orchestrator::OrchestratorHandle;

use crate::api::{issue_view, ApiError, ApiErrorBody, StateView};

#[derive(Clone)]
struct AppState {
    handle: OrchestratorHandle,
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
pub async fn serve(addr: SocketAddr, handle: OrchestratorHandle) -> std::io::Result<ServerHandle> {
    let state = AppState { handle };
    let router = Router::new()
        .route("/", get(dashboard_html))
        .route("/api/v1/state", get(get_state))
        .route("/api/v1/refresh", post(post_refresh))
        .route("/api/v1/:identifier", get(get_issue))
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
                Html(String::from("<h1>orchestrator unavailable</h1>")),
            );
        }
    };
    let view = StateView::from_snapshot(&snap, None);
    let html = render_dashboard(&view);
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

fn render_dashboard(view: &StateView) -> String {
    let mut html = String::new();
    html.push_str("<!doctype html><html><head><meta charset=\"utf-8\"><title>Symphony</title>");
    html.push_str("<style>body{font-family:system-ui;margin:2rem}table{border-collapse:collapse;width:100%}th,td{border:1px solid #ccc;padding:.4rem .6rem;text-align:left}h2{margin-top:2rem}</style>");
    html.push_str("</head><body>");
    html.push_str(&format!(
        "<h1>Symphony</h1><p>generated_at: {} &middot; running: {} &middot; retrying: {}</p>",
        view.generated_at, view.counts.running, view.counts.retrying
    ));
    html.push_str(&format!(
        "<p>tokens in/out/total: {} / {} / {} &middot; runtime: {:.1}s</p>",
        view.codex_totals.input_tokens,
        view.codex_totals.output_tokens,
        view.codex_totals.total_tokens,
        view.codex_totals.seconds_running
    ));

    html.push_str("<h2>running</h2>");
    if view.running.is_empty() {
        html.push_str("<p>no active sessions</p>");
    } else {
        html.push_str("<table><tr><th>identifier</th><th>state</th><th>turns</th><th>last event</th><th>tokens</th></tr>");
        for r in &view.running {
            html.push_str(&format!(
                "<tr><td>{}</td><td>{}</td><td>{}</td><td>{}</td><td>{}</td></tr>",
                escape(&r.issue_identifier),
                escape(&r.state),
                r.turn_count,
                escape(r.last_event.as_deref().unwrap_or("")),
                r.tokens.total_tokens,
            ));
        }
        html.push_str("</table>");
    }

    html.push_str("<h2>retrying</h2>");
    if view.retrying.is_empty() {
        html.push_str("<p>no retries pending</p>");
    } else {
        html.push_str(
            "<table><tr><th>identifier</th><th>attempt</th><th>due_at</th><th>error</th></tr>",
        );
        for r in &view.retrying {
            html.push_str(&format!(
                "<tr><td>{}</td><td>{}</td><td>{}</td><td>{}</td></tr>",
                escape(&r.issue_identifier),
                r.attempt,
                escape(&r.due_at),
                escape(r.error.as_deref().unwrap_or("")),
            ));
        }
        html.push_str("</table>");
    }
    html.push_str("</body></html>");
    html
}

fn escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
}
