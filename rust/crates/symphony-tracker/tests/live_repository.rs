//! Live repository smoke: explicitly opt in with --ignored and tracker credentials.
use symphony_core::config::{RepositoryConfig, TrackerConfig, TrackerKind};
use symphony_tracker::{repository::RepositoryClient, Tracker};

fn required(name: &str) -> String {
    std::env::var(name)
        .ok()
        .filter(|s| !s.trim().is_empty())
        .unwrap_or_else(|| panic!("opted-in GitHub smoke requires {name}"))
}

#[tokio::test]
#[ignore = "real GitHub API; requires GITHUB_TOKEN, GITHUB_OWNER, GITHUB_REPO"]
async fn live_github_reads_and_reconciles() {
    smoke(TrackerKind::Github, "GITHUB").await;
}

#[tokio::test]
#[ignore = "real Forgejo API; requires FORGEJO_TOKEN, FORGEJO_OWNER, FORGEJO_REPO, FORGEJO_API_URL"]
async fn live_forgejo_reads_and_reconciles() {
    smoke(TrackerKind::Forgejo, "FORGEJO").await;
}

async fn smoke(kind: TrackerKind, prefix: &str) {
    let endpoint = if kind == TrackerKind::Github {
        std::env::var("GITHUB_API_URL").unwrap_or_else(|_| "https://api.github.com".into())
    } else {
        required("FORGEJO_API_URL")
    };
    let config = TrackerConfig {
        kind,
        endpoint,
        api_key: None,
        project_slug: None,
        active_states: vec!["open".into()],
        terminal_states: vec!["Closed".into()],
        repository: Some(RepositoryConfig {
            owner: required(&format!("{prefix}_OWNER")),
            repo: required(&format!("{prefix}_REPO")),
            api_token: Some(required(&format!("{prefix}_TOKEN"))),
            ..Default::default()
        }),
    };
    let client = RepositoryClient::new(&config).unwrap();
    let issues = client
        .fetch_candidate_issues()
        .await
        .expect("candidate fetch");
    let ids = issues
        .iter()
        .take(5)
        .map(|issue| issue.id.clone())
        .collect::<Vec<_>>();
    let states = client
        .fetch_issue_states_by_ids(&ids)
        .await
        .expect("reconciliation");
    for state in states {
        assert!(ids.contains(&state.id));
    }
    client
        .fetch_issues_by_states(&["Closed".into()])
        .await
        .expect("terminal cleanup fetch");
}
