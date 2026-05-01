//! SPEC §17.8 Real Integration Profile — opt-in smoke test against live
//! Linear. Skipped by default; enable with
//! `cargo test -p symphony-tracker --test live_linear -- --ignored` and
//! supply `LINEAR_API_KEY` (and optionally `LINEAR_PROJECT_SLUG`).
//!
//! - `live_linear_viewer_via_tool` exercises the `linear_graphql` extension
//!   against the real Linear endpoint with the user's actual API token.
//! - `live_linear_candidate_fetch` runs the configured candidate-issue query
//!   when `LINEAR_PROJECT_SLUG` is also set.

use std::sync::Arc;

use serde_json::json;
use symphony_codex::tools::ToolExecutor;
use symphony_tracker::linear::{LinearClient, LinearConfig, ReqwestTransport};
use symphony_tracker::linear_tool::LinearGraphqlTool;
use symphony_tracker::Tracker;

const ENDPOINT: &str = "https://api.linear.app/graphql";

fn api_key() -> String {
    std::env::var("LINEAR_API_KEY").unwrap_or_else(|_| {
        panic!(
            "live Linear smoke requires LINEAR_API_KEY in the environment; \
             this test was explicitly opted in via --ignored"
        )
    })
}

#[tokio::test]
#[ignore = "real Linear API; run with --ignored and LINEAR_API_KEY set"]
async fn live_linear_viewer_via_tool() {
    let key = api_key();
    let transport = Arc::new(ReqwestTransport::new(ENDPOINT.into(), key));
    let tool = LinearGraphqlTool::new(transport);
    let result = tool
        .execute(
            "linear_graphql",
            &json!({"query": "query { viewer { id email name } }"}),
        )
        .await;
    assert!(
        result.success,
        "linear_graphql viewer query should succeed: {:?}",
        result.output
    );
    let id = result
        .output
        .pointer("/data/viewer/id")
        .and_then(|v| v.as_str());
    assert!(
        id.is_some(),
        "expected data.viewer.id in: {:?}",
        result.output
    );
}

#[tokio::test]
#[ignore = "real Linear API; run with --ignored and LINEAR_API_KEY + LINEAR_PROJECT_SLUG set"]
async fn live_linear_candidate_fetch() {
    let key = api_key();
    let project_slug = std::env::var("LINEAR_PROJECT_SLUG").unwrap_or_else(|_| {
        panic!(
            "live Linear candidate-fetch smoke requires LINEAR_PROJECT_SLUG; \
             set the slug of a Linear project the API key has access to"
        )
    });
    let active_states = std::env::var("SYMPHONY_LIVE_ACTIVE_STATES")
        .ok()
        .map(|s| s.split(',').map(str::to_string).collect())
        .unwrap_or_else(|| vec!["Todo".into(), "In Progress".into()]);

    let cfg = LinearConfig {
        endpoint: ENDPOINT.into(),
        api_key: key,
        project_slug,
        active_states,
        terminal_states: vec!["Done".into(), "Cancelled".into()],
    };
    let client = LinearClient::new(cfg).expect("LinearClient");
    let issues = client
        .fetch_candidate_issues()
        .await
        .expect("candidate fetch");
    // Even an empty result is a success — we only need to confirm the query
    // parses, the auth works, and the response normalizes.
    for issue in &issues {
        assert!(!issue.id.is_empty(), "issue.id must be non-empty");
        assert!(
            !issue.identifier.is_empty(),
            "issue.identifier must be non-empty"
        );
    }
    eprintln!("live Linear smoke: fetched {} issue(s)", issues.len());
}
