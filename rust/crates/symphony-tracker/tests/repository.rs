use axum::{
    extract::{OriginalUri, State},
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
    routing::any,
    Json, Router,
};
use base64::{engine::general_purpose::URL_SAFE_NO_PAD, Engine};
use serde_json::{json, Value};
use std::{
    collections::BTreeMap,
    sync::{Arc, Mutex},
};
use symphony_core::config::{RepositoryConfig, TrackerConfig, TrackerKind};
use symphony_tracker::{
    factory::build_tracker, repository::RepositoryClient, Tracker, TrackerError,
};

#[derive(Clone)]
struct Request {
    uri: String,
    headers: HeaderMap,
    body: Value,
}
struct Reply {
    status: StatusCode,
    body: Value,
    link: Option<String>,
}
type Responder = dyn Fn(&Request) -> Reply + Send + Sync;

struct Server {
    endpoint: String,
    requests: Arc<Mutex<Vec<Request>>>,
    task: tokio::task::JoinHandle<()>,
}
impl Drop for Server {
    fn drop(&mut self) {
        self.task.abort();
    }
}

impl Server {
    async fn new(responder: impl Fn(&Request) -> Reply + Send + Sync + 'static) -> Self {
        let requests = Arc::new(Mutex::new(Vec::new()));
        type MockState = (Arc<Mutex<Vec<Request>>>, Arc<Responder>);
        async fn handler(
            State((requests, respond)): State<MockState>,
            OriginalUri(uri): OriginalUri,
            headers: HeaderMap,
            body: axum::body::Bytes,
        ) -> Response {
            let request = Request {
                uri: uri.to_string(),
                headers,
                body: serde_json::from_slice(&body).unwrap_or(Value::Null),
            };
            let reply = respond(&request);
            requests.lock().unwrap().push(request);
            let mut response = (reply.status, Json(reply.body)).into_response();
            if reply.status == StatusCode::FOUND {
                response
                    .headers_mut()
                    .insert("location", "http://outside.invalid/steal".parse().unwrap());
            }
            if let Some(link) = reply.link {
                response.headers_mut().insert("link", link.parse().unwrap());
            }
            response
        }
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let endpoint = format!(
            "http://{}/enterprise/api/v3",
            listener.local_addr().unwrap()
        );
        let router = Router::new()
            .fallback(any(handler))
            .with_state((requests.clone(), Arc::new(responder) as Arc<Responder>));
        let task = tokio::spawn(async move {
            axum::serve(listener, router).await.unwrap();
        });
        Self {
            endpoint,
            requests,
            task,
        }
    }
    fn config(&self) -> TrackerConfig {
        TrackerConfig {
            kind: TrackerKind::Github,
            endpoint: self.endpoint.clone(),
            api_key: None,
            project_slug: None,
            active_states: vec!["Todo".into(), "In Progress".into()],
            terminal_states: vec!["Done".into(), "Closed".into()],
            repository: Some(RepositoryConfig {
                owner: "acme".into(),
                repo: "widgets".into(),
                api_token: Some("test-credential".into()),
                label_priority_map: BTreeMap::from([("urgent".into(), 0), ("later".into(), 3)]),
                ..Default::default()
            }),
        }
    }
}
fn ok(body: Value) -> Reply {
    Reply {
        status: StatusCode::OK,
        body,
        link: None,
    }
}
fn issue(number: u64, state: &str, labels: &[&str]) -> Value {
    json!({"number": number, "title": "Fix login flow!", "body": "Keep **this** body\nunchanged.", "state": state,
        "html_url": format!("https://github.com/acme/widgets/issues/{number}"),
        "labels": labels.iter().map(|name| json!({"name": name})).collect::<Vec<_>>(),
        "assignees": [{"login": "alice"}], "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-02T12:00:00Z"})
}

#[tokio::test]
async fn paginates_normalizes_filters_and_keeps_terminal_cleanup_unassigned() {
    let server = Server::new(|request| {
        if request.uri.ends_with("page=1") {
            let mut pr = issue(2, "open", &["Todo"]);
            pr["pull_request"] = json!({"url": "ignored"});
            Reply {
                status: StatusCode::OK,
                body: json!([issue(1, "open", &["TODO", "URGENT", "later"]), pr]),
                // The host must never be contacted; pagination uses the configured endpoint.
                link: Some("<https://outside.invalid/steal?page=2>; rel=\"next\"".into()),
            }
        } else {
            let mut unassigned = issue(5, "closed", &["Todo"]);
            unassigned["assignees"] = json!([]);
            ok(json!([
                issue(3, "open", &[]),
                issue(4, "open", &["Todo", "DONE"]),
                unassigned
            ]))
        }
    })
    .await;
    let mut config = server.config();
    config.repository.as_mut().unwrap().assignee = Some("Alice".into());
    let (client, tools) = build_tracker(&config).unwrap();
    assert!(tools.specs().is_empty());
    let candidates = client.fetch_candidate_issues().await.unwrap();
    assert_eq!(candidates.len(), 1);
    let first = &candidates[0];
    assert_eq!(first.identifier, "acme/widgets#1");
    assert_eq!(first.state, "Todo");
    assert_eq!(first.priority, Some(0));
    assert_eq!(first.labels, vec!["todo", "urgent", "later"]);
    assert_eq!(
        first.description.as_deref(),
        Some("Keep **this** body\nunchanged.")
    );
    assert_eq!(first.branch_name.as_deref(), Some("1-fix-login-flow"));
    assert!(first.created_at.is_some());
    assert!(first.updated_at.is_some());
    assert!(first.blocked_by.is_empty());
    let terminal = client
        .fetch_issues_by_states(&["Done".into(), "Closed".into()])
        .await
        .unwrap();
    assert_eq!(
        terminal
            .iter()
            .map(|i| i.state.as_str())
            .collect::<Vec<_>>(),
        vec!["Done", "Closed"]
    );
    let requests = server.requests.lock().unwrap();
    assert_eq!(requests.len(), 4);
    for request in requests.iter() {
        assert!(request
            .uri
            .starts_with("/enterprise/api/v3/repos/acme/widgets/issues?"));
        assert!(request.uri.contains("state=all"));
        assert!(!request.uri.contains("labels="));
        assert!(!request.uri.contains("test-credential"));
        assert_eq!(request.headers["authorization"], "Bearer test-credential");
        assert_eq!(request.headers["user-agent"], "symphony/0.1");
    }
}

#[tokio::test]
async fn literal_open_fallback_requires_explicit_configuration() {
    let server = Server::new(|_| {
        ok(json!([
            issue(1, "open", &[]),
            issue(2, "closed", &["Todo"])
        ]))
    })
    .await;
    let mut config = server.config();
    config.active_states = vec!["open".into()];
    config.terminal_states = vec!["Finished".into()];
    let client = RepositoryClient::new(&config).unwrap();
    let candidates = client.fetch_candidate_issues().await.unwrap();
    assert_eq!(candidates.len(), 1);
    assert_eq!(candidates[0].state, "open");
    assert_eq!(candidates[0].priority, Some(1));
    assert_eq!(
        client
            .fetch_issues_by_states(&["Finished".into()])
            .await
            .unwrap()[0]
            .state,
        "Finished"
    );
}

#[tokio::test]
async fn reconciliation_survives_restart_and_assignee_changes_but_not_api_errors() {
    let server = Server::new(|r| {
        if r.uri.contains('?') {
            return ok(json!([issue(1, "open", &["Todo"])]));
        }
        if r.uri.ends_with("/2") {
            return Reply {
                status: StatusCode::NOT_FOUND,
                body: json!({}),
                link: None,
            };
        }
        if r.uri.ends_with("/3") {
            return Reply {
                status: StatusCode::FORBIDDEN,
                body: json!({"message":"test-credential"}),
                link: None,
            };
        }
        let mut changed = issue(1, "closed", &["Todo"]);
        changed["assignees"] = json!([]);
        ok(changed)
    })
    .await;
    let config = server.config();
    let id = RepositoryClient::new(&config)
        .unwrap()
        .fetch_candidate_issues()
        .await
        .unwrap()[0]
        .id
        .clone();
    let mut restarted = config;
    restarted.repository.as_mut().unwrap().assignee = Some("alice".into());
    let client = RepositoryClient::new(&restarted).unwrap();
    let missing = format!("{}2", id.trim_end_matches('1'));
    let states = client
        .fetch_issue_states_by_ids(&[id.clone(), missing])
        .await
        .unwrap();
    assert_eq!(states.len(), 1);
    assert_eq!(states[0].id, id);
    assert_eq!(states[0].state, "Closed");
    let forbidden = format!("{}3", id.trim_end_matches('1'));
    let error = client
        .fetch_issue_states_by_ids(&[forbidden])
        .await
        .unwrap_err();
    assert!(matches!(
        error,
        TrackerError::RepositoryApiStatus("github", 403)
    ));
    assert!(!error.to_string().contains("test-credential"));
    let before = server.requests.lock().unwrap().len();
    assert!(client
        .fetch_issue_states_by_ids(&["foreign-repository/1".into()])
        .await
        .is_err());
    assert_eq!(server.requests.lock().unwrap().len(), before);
    assert!(client.fetch_issues_by_states(&[]).await.unwrap().is_empty());
    assert!(client
        .fetch_issue_states_by_ids(&[])
        .await
        .unwrap()
        .is_empty());
    assert_eq!(server.requests.lock().unwrap().len(), before);
}

#[tokio::test]
async fn later_page_failure_and_repeated_pages_are_errors() {
    for repeated in [false, true] {
        let server = Server::new(move |r| {
            if !repeated && r.uri.ends_with("page=2") {
                return Reply {
                    status: StatusCode::TOO_MANY_REQUESTS,
                    body: json!({}),
                    link: None,
                };
            }
            Reply {
                status: StatusCode::OK,
                body: json!([issue(1, "open", &["Todo"])]),
                link: Some("<https://github.invalid/issues?page=2>; rel=\"next\"".into()),
            }
        })
        .await;
        let client = RepositoryClient::new(&server.config()).unwrap();
        assert!(client.fetch_candidate_issues().await.is_err());
        assert_eq!(server.requests.lock().unwrap().len(), 2);
    }
}

#[tokio::test]
async fn pagination_uses_advertised_page_and_rejects_invalid_links() {
    let server = Server::new(|r| {
        if r.uri.ends_with("page=1") {
            Reply {
                status: StatusCode::OK,
                body: json!([issue(1, "open", &["Todo"])]),
                link: Some("<https://outside.invalid/issues?page=3>; rel=\"next\"".into()),
            }
        } else {
            assert!(r.uri.ends_with("page=3"));
            ok(json!([issue(3, "open", &["Todo"])]))
        }
    })
    .await;
    assert_eq!(
        RepositoryClient::new(&server.config())
            .unwrap()
            .fetch_candidate_issues()
            .await
            .unwrap()
            .len(),
        2
    );
    for link in [
        "<https://outside.invalid/issues?page=1>; rel=\"next\"",
        "<https://outside.invalid/issues>; rel=\"next\"",
        "invalid-link",
    ] {
        let server = Server::new(move |_| Reply {
            status: StatusCode::OK,
            body: json!([issue(1, "open", &["Todo"])]),
            link: Some(link.into()),
        })
        .await;
        assert!(RepositoryClient::new(&server.config())
            .unwrap()
            .fetch_candidate_issues()
            .await
            .is_err());
    }
}

#[tokio::test]
async fn redirects_and_malformed_payloads_fail_without_exposing_credentials() {
    for (status, body) in [
        (StatusCode::FOUND, json!({})),
        (StatusCode::OK, json!({"token":"test-credential"})),
        (StatusCode::OK, json!([issue(0, "open", &["Todo"])])),
    ] {
        let server = Server::new(move |_| Reply {
            status,
            body: body.clone(),
            link: None,
        })
        .await;
        let error = RepositoryClient::new(&server.config())
            .unwrap()
            .fetch_candidate_issues()
            .await
            .unwrap_err();
        assert!(!error.to_string().contains("test-credential"));
        if status == StatusCode::FOUND {
            assert!(matches!(
                error,
                TrackerError::RepositoryApiStatus("github", 302)
            ));
            assert_eq!(server.requests.lock().unwrap().len(), 1);
        }
    }
}

#[tokio::test]
async fn app_auth_signs_jwt_refreshes_on_401_and_caches_installation_token() {
    // Generate an ephemeral test key rather than commit private-key material.
    let output = std::process::Command::new("openssl")
        .args([
            "genpkey",
            "-algorithm",
            "RSA",
            "-pkeyopt",
            "rsa_keygen_bits:2048",
        ])
        .output()
        .expect("App auth test requires OpenSSL (available on CI Ubuntu)");
    assert!(output.status.success());
    let key = String::from_utf8(output.stdout).unwrap();
    let exchanges = Arc::new(Mutex::new(0usize));
    let counted = exchanges.clone();
    let server = Server::new(move |r| {
        if r.uri.ends_with("/app/installations/42/access_tokens") {
            let mut count = counted.lock().unwrap(); *count += 1;
            let jwt = r.headers["authorization"].to_str().unwrap().strip_prefix("Bearer ").unwrap();
            let parts = jwt.split('.').collect::<Vec<_>>(); assert_eq!(parts.len(),3);
            let header:Value=serde_json::from_slice(&URL_SAFE_NO_PAD.decode(parts[0]).unwrap()).unwrap();
            let claims:Value=serde_json::from_slice(&URL_SAFE_NO_PAD.decode(parts[1]).unwrap()).unwrap();
            assert_eq!(header["alg"],"RS256"); assert_eq!(claims["iss"],"123");
            assert_eq!(claims["exp"].as_i64().unwrap()-claims["iat"].as_i64().unwrap(),600);
            assert_eq!(URL_SAFE_NO_PAD.decode(parts[2]).unwrap().len(),256);
            assert_eq!(r.body,json!({"repositories":["widgets"],"permissions":{"issues":"read"}}));
            return ok(json!({"token":format!("installation-{count}"),"expires_at":"2099-01-01T00:00:00Z"}));
        }
        if r.headers["authorization"] == "Bearer installation-1" {
            return Reply { status:StatusCode::UNAUTHORIZED,body:json!({}),link:None };
        }
        assert_eq!(r.headers["authorization"],"Bearer installation-2");
        ok(json!([issue(1,"open", &["Todo"])]))
    }).await;
    let mut config = server.config();
    let auth = config.repository.as_mut().unwrap();
    auth.app_id = Some("123".into());
    auth.app_installation_id = Some("42".into());
    auth.private_key = Some(key);
    let client = RepositoryClient::new(&config).unwrap();
    assert_eq!(client.fetch_candidate_issues().await.unwrap().len(), 1);
    assert_eq!(client.fetch_candidate_issues().await.unwrap().len(), 1);
    assert_eq!(*exchanges.lock().unwrap(), 2);
    assert!(!format!("{config:?}").contains("test-credential"));
}

#[tokio::test]
async fn invalid_endpoints_and_partial_app_credentials_fail_before_requests() {
    let server = Server::new(|_| panic!("invalid config must not make requests")).await;
    for endpoint in [
        "file:///tmp/issues",
        "https://user:password@example.com",
        "https://example.com?token=x",
        "https://example.com#fragment",
    ] {
        let mut config = server.config();
        config.endpoint = endpoint.into();
        assert!(RepositoryClient::new(&config).is_err());
    }
    let mut config = server.config();
    config.repository.as_mut().unwrap().app_id = Some("123".into());
    assert!(RepositoryClient::new(&config).is_err());
    config.repository.as_mut().unwrap().app_installation_id = Some("42".into());
    config.repository.as_mut().unwrap().private_key = Some("invalid-private-material".into());
    assert!(RepositoryClient::new(&config).is_err());
    assert!(server.requests.lock().unwrap().is_empty());
}

#[tokio::test]
async fn forgejo_uses_instance_prefix_token_auth_and_server_pagination() {
    let server = Server::new(|r| {
        assert!(r.uri.starts_with("/forge/api/v1/repos/acme/widgets/issues"));
        assert_eq!(r.headers["authorization"], "token test-credential");
        assert_eq!(r.headers["accept"], "application/json");
        assert!(!r.headers.contains_key("x-github-api-version"));
        if !r.uri.contains('?') {
            let mut closed = issue(1, "closed", &["Todo"]);
            closed["assignees"] = Value::Null;
            closed["labels"] = Value::Null;
            return ok(closed);
        }
        assert!(r.uri.contains("type=issues"));
        assert!(r.uri.contains("limit=50"));
        assert!(!r.uri.contains("per_page="));
        assert!(!r.uri.contains("labels="));
        if r.uri.ends_with("page=1") {
            let mut pr = issue(2, "open", &["Todo"]);
            pr["pull_request"] = json!({});
            Reply {
                status: StatusCode::OK,
                body: json!([issue(1, "open", &["TODO", "urgent"]), pr]),
                link: Some(
                    "<https://forge.invalid/api/v1/repos/acme/widgets/issues?page=2>; rel=\"next\""
                        .into(),
                ),
            }
        } else {
            ok(json!([
                issue(3, "open", &["Done"]),
                issue(4, "closed", &[])
            ]))
        }
    })
    .await;
    let mut config = server.config();
    config.kind = TrackerKind::Forgejo;
    config.endpoint = server
        .endpoint
        .replace("/enterprise/api/v3", "/forge/api/v1");
    let client = RepositoryClient::new(&config).unwrap();
    let candidates = client.fetch_candidate_issues().await.unwrap();
    assert_eq!(candidates.len(), 1);
    assert_eq!(candidates[0].priority, Some(0));
    assert!(candidates[0].id.starts_with("forgejo:"));
    let terminal = client
        .fetch_issues_by_states(&["Done".into(), "Closed".into()])
        .await
        .unwrap();
    assert_eq!(terminal.len(), 2);
    let restarted = RepositoryClient::new(&config).unwrap();
    let states = restarted
        .fetch_issue_states_by_ids(&[candidates[0].id.clone()])
        .await
        .unwrap();
    assert_eq!(states[0].state, "Closed");
    config.endpoint = server.endpoint.clone();
    assert!(RepositoryClient::new(&config).is_err());
}
