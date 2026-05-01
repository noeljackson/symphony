//! axum-based HTTP server. Exposes the SPEC §13.7 dashboard + JSON API.

use std::convert::Infallible;
use std::net::SocketAddr;
use std::time::Duration;

use axum::{
    extract::{Path, State},
    http::StatusCode,
    response::{
        sse::{Event, KeepAlive, Sse},
        Html, IntoResponse, Json,
    },
    routing::{get, post},
    Router,
};
use futures::stream::Stream;
use symphony_orchestrator::{EventBroadcast, OrchestratorHandle};
use tokio_stream::wrappers::BroadcastStream;
use tokio_stream::StreamExt;

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
        .route("/api/v1/events", get(get_events))
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
    let initial = serde_json::to_string(view).unwrap_or_else(|_| "{}".into());
    format!(
        r#"<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>Symphony</title>
<style>
body {{ font-family: system-ui; margin: 2rem; }}
table {{ border-collapse: collapse; width: 100%; }}
th, td {{ border: 1px solid #ccc; padding: .4rem .6rem; text-align: left; }}
h2 {{ margin-top: 2rem; }}
#events {{ max-height: 18rem; overflow-y: auto; font-family: ui-monospace, monospace; font-size: .85rem; background: #f7f7f7; padding: .5rem; border: 1px solid #ddd; }}
.badge {{ display: inline-block; padding: 0 .4rem; border-radius: .3rem; background: #e8f0fe; color: #1a73e8; font-size: .75rem; }}
.lag {{ background: #fbe9e7; color: #c62828; }}
</style>
</head>
<body>
<h1>Symphony <span id="conn" class="badge">connecting…</span></h1>
<p id="header"></p>
<p id="totals"></p>
<h2>running</h2>
<div id="running"></div>
<h2>retrying</h2>
<div id="retrying"></div>
<h2>events</h2>
<div id="events"></div>
<script>
const initial = {initial};
const $ = (id) => document.getElementById(id);
const escape = (s) => String(s ?? '').replace(/[&<>]/g, (c) => ({{'&':'&amp;','<':'&lt;','>':'&gt;'}}[c]));

function render(view) {{
  $('header').textContent = `generated_at: ${{view.generated_at}} · running: ${{view.counts.running}} · retrying: ${{view.counts.retrying}}`;
  const t = view.agent_totals || {{}};
  $('totals').textContent = `tokens in/out/total: ${{t.input_tokens ?? 0}} / ${{t.output_tokens ?? 0}} / ${{t.total_tokens ?? 0}} · runtime: ${{(t.seconds_running ?? 0).toFixed(1)}}s`;

  if (!view.running?.length) {{
    $('running').innerHTML = '<p>no active sessions</p>';
  }} else {{
    let h = '<table><tr><th>identifier</th><th>state</th><th>turns</th><th>last event</th><th>tokens</th></tr>';
    for (const r of view.running) {{
      h += `<tr><td>${{escape(r.issue_identifier)}}</td><td>${{escape(r.state)}}</td><td>${{r.turn_count ?? 0}}</td><td>${{escape(r.last_event)}}</td><td>${{r.tokens?.total_tokens ?? 0}}</td></tr>`;
    }}
    h += '</table>';
    $('running').innerHTML = h;
  }}

  if (!view.retrying?.length) {{
    $('retrying').innerHTML = '<p>no retries pending</p>';
  }} else {{
    let h = '<table><tr><th>identifier</th><th>attempt</th><th>due_at</th><th>error</th></tr>';
    for (const r of view.retrying) {{
      h += `<tr><td>${{escape(r.issue_identifier)}}</td><td>${{r.attempt}}</td><td>${{escape(r.due_at)}}</td><td>${{escape(r.error)}}</td></tr>`;
    }}
    h += '</table>';
    $('retrying').innerHTML = h;
  }}
}}

function snapshot() {{
  return fetch('/api/v1/state').then((r) => r.json()).then(render);
}}

let view = initial;
render(view);

const log = (cls, text) => {{
  const div = document.createElement('div');
  if (cls) div.className = cls;
  div.textContent = text;
  $('events').prepend(div);
  while ($('events').children.length > 200) $('events').lastChild.remove();
}};

const es = new EventSource('/api/v1/events');
es.onopen = () => {{ $('conn').textContent = 'live'; $('conn').classList.remove('lag'); }};
es.onerror = () => {{ $('conn').textContent = 'reconnecting…'; $('conn').classList.add('lag'); }};
es.addEventListener('snapshot', (ev) => {{ try {{ view = JSON.parse(ev.data); render(view); }} catch (_) {{}} }});
es.addEventListener('lagged', (ev) => {{ log('lag', `[lagged] ${{ev.data}} — re-snapshotting`); snapshot(); }});
['session_started','turn_completed','turn_failed','turn_cancelled','notification','approval_auto_approved','tool_call_completed','tool_call_failed','unsupported_tool_call','malformed','other_message','startup_failed','turn_input_required'].forEach((kind) => {{
  es.addEventListener(kind, (ev) => {{
    try {{
      const data = JSON.parse(ev.data);
      log(null, `[${{data.timestamp ?? ''}}] ${{data.issue_identifier ?? '?'}} · ${{kind}}${{data.message ? ' · ' + data.message : ''}}`);
      snapshot();
    }} catch (_) {{}}
  }});
}});
</script>
</body>
</html>
"#
    )
}
