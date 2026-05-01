//! SPEC §11.2 Linear GraphQL adapter.
//!
//! The transport is abstracted behind [`GraphqlTransport`] so tests can drive
//! the adapter deterministically without a live network. The default
//! implementation uses `reqwest`.

use std::sync::Arc;

use async_trait::async_trait;
use serde_json::{json, Value};
use time::OffsetDateTime;

use symphony_core::issue::{Blocker, Issue};

use crate::errors::TrackerError;
use crate::tracker::{IssueState, Tracker};

const PAGE_SIZE: u32 = 50;
const NETWORK_TIMEOUT_MS: u64 = 30_000;

const ISSUE_QUERY: &str = r#"
query SymphonyLinearPoll($projectSlug: String!, $stateNames: [String!]!, $first: Int!, $relationFirst: Int!, $after: String) {
  issues(filter: {project: {slugId: {eq: $projectSlug}}, state: {name: {in: $stateNames}}}, first: $first, after: $after) {
    nodes {
      id
      identifier
      title
      description
      priority
      state { name }
      branchName
      url
      labels { nodes { name } }
      inverseRelations(first: $relationFirst) {
        nodes {
          type
          issue { id identifier state { name } }
        }
      }
      createdAt
      updatedAt
    }
    pageInfo { hasNextPage endCursor }
  }
}
"#;

const ISSUES_BY_ID_QUERY: &str = r#"
query SymphonyLinearIssuesById($ids: [ID!]!, $first: Int!, $relationFirst: Int!) {
  issues(filter: {id: {in: $ids}}, first: $first) {
    nodes {
      id
      identifier
      title
      description
      priority
      state { name }
      branchName
      url
      labels { nodes { name } }
      inverseRelations(first: $relationFirst) {
        nodes {
          type
          issue { id identifier state { name } }
        }
      }
      createdAt
      updatedAt
    }
  }
}
"#;

#[derive(Debug, Clone)]
pub struct LinearConfig {
    pub endpoint: String,
    pub api_key: String,
    pub project_slug: String,
    pub active_states: Vec<String>,
    pub terminal_states: Vec<String>,
}

#[async_trait]
pub trait GraphqlTransport: Send + Sync {
    async fn post(&self, query: &str, variables: Value) -> Result<Value, TrackerError>;
}

pub struct ReqwestTransport {
    client: reqwest::Client,
    endpoint: String,
    auth: String,
}

impl ReqwestTransport {
    pub fn new(endpoint: String, auth: String) -> Self {
        let client = reqwest::Client::builder()
            .timeout(std::time::Duration::from_millis(NETWORK_TIMEOUT_MS))
            .build()
            .expect("reqwest client");
        Self {
            client,
            endpoint,
            auth,
        }
    }
}

#[async_trait]
impl GraphqlTransport for ReqwestTransport {
    async fn post(&self, query: &str, variables: Value) -> Result<Value, TrackerError> {
        let body = json!({ "query": query, "variables": variables });
        let resp = self
            .client
            .post(&self.endpoint)
            .header("Authorization", &self.auth)
            .header("Content-Type", "application/json")
            .json(&body)
            .send()
            .await
            .map_err(|e| TrackerError::LinearApiRequest(e.to_string()))?;

        let status = resp.status();
        if !status.is_success() {
            return Err(TrackerError::LinearApiStatus(status.as_u16()));
        }
        resp.json::<Value>()
            .await
            .map_err(|e| TrackerError::LinearApiRequest(e.to_string()))
    }
}

pub struct LinearClient {
    cfg: LinearConfig,
    transport: Arc<dyn GraphqlTransport>,
}

impl LinearClient {
    pub fn new(cfg: LinearConfig) -> Result<Self, TrackerError> {
        if cfg.api_key.is_empty() {
            return Err(TrackerError::MissingTrackerApiKey);
        }
        if cfg.project_slug.is_empty() {
            return Err(TrackerError::MissingTrackerProjectSlug);
        }
        let transport = Arc::new(ReqwestTransport::new(
            cfg.endpoint.clone(),
            cfg.api_key.clone(),
        ));
        Ok(Self { cfg, transport })
    }

    /// Test-only constructor that swaps the transport.
    pub fn with_transport(cfg: LinearConfig, transport: Arc<dyn GraphqlTransport>) -> Self {
        Self { cfg, transport }
    }

    async fn fetch_paginated_by_states(
        &self,
        state_names: &[String],
    ) -> Result<Vec<Issue>, TrackerError> {
        let mut after: Option<String> = None;
        let mut acc = Vec::new();
        loop {
            let variables = json!({
                "projectSlug": self.cfg.project_slug,
                "stateNames": state_names,
                "first": PAGE_SIZE,
                "relationFirst": PAGE_SIZE,
                "after": after,
            });
            let body = self.transport.post(ISSUE_QUERY, variables).await?;
            let (page, page_info) = decode_page_response(&body)?;
            acc.extend(page);
            match next_cursor(&page_info)? {
                Some(cursor) => after = Some(cursor),
                None => break,
            }
        }
        Ok(acc)
    }
}

#[async_trait]
impl Tracker for LinearClient {
    async fn fetch_candidate_issues(&self) -> Result<Vec<Issue>, TrackerError> {
        self.fetch_paginated_by_states(&self.cfg.active_states)
            .await
    }

    async fn fetch_issues_by_states(
        &self,
        state_names: &[String],
    ) -> Result<Vec<Issue>, TrackerError> {
        let mut deduped: Vec<String> = Vec::new();
        for s in state_names {
            if !deduped.iter().any(|x| x == s) {
                deduped.push(s.clone());
            }
        }
        if deduped.is_empty() {
            return Ok(Vec::new());
        }
        self.fetch_paginated_by_states(&deduped).await
    }

    async fn fetch_issue_states_by_ids(
        &self,
        issue_ids: &[String],
    ) -> Result<Vec<IssueState>, TrackerError> {
        let mut deduped: Vec<String> = Vec::new();
        for id in issue_ids {
            if !deduped.iter().any(|x| x == id) {
                deduped.push(id.clone());
            }
        }
        if deduped.is_empty() {
            return Ok(Vec::new());
        }

        let mut acc: Vec<Issue> = Vec::new();
        let mut start = 0;
        while start < deduped.len() {
            let end = (start + PAGE_SIZE as usize).min(deduped.len());
            let batch = &deduped[start..end];
            let variables = json!({
                "ids": batch,
                "first": batch.len(),
                "relationFirst": PAGE_SIZE,
            });
            let body = self.transport.post(ISSUES_BY_ID_QUERY, variables).await?;
            let issues = decode_response(&body)?;
            acc.extend(issues);
            start = end;
        }

        // Preserve the caller's request order.
        acc.sort_by_key(|issue| {
            deduped
                .iter()
                .position(|id| id == &issue.id)
                .unwrap_or(usize::MAX)
        });

        let states = acc
            .into_iter()
            .map(|i| IssueState {
                id: i.id,
                identifier: i.identifier,
                state: i.state,
            })
            .collect();
        Ok(states)
    }
}

#[derive(Debug, Clone)]
struct PageInfo {
    has_next_page: bool,
    end_cursor: Option<String>,
}

fn decode_response(body: &Value) -> Result<Vec<Issue>, TrackerError> {
    if let Some(errors) = body.get("errors") {
        return Err(TrackerError::LinearGraphqlErrors(errors.to_string()));
    }
    let nodes = body
        .get("data")
        .and_then(|d| d.get("issues"))
        .and_then(|i| i.get("nodes"))
        .and_then(|n| n.as_array())
        .ok_or_else(|| TrackerError::LinearUnknownPayload(summarize(body)))?;

    Ok(nodes.iter().filter_map(normalize_issue).collect())
}

fn decode_page_response(body: &Value) -> Result<(Vec<Issue>, PageInfo), TrackerError> {
    if let Some(errors) = body.get("errors") {
        return Err(TrackerError::LinearGraphqlErrors(errors.to_string()));
    }
    let issues_obj = body
        .get("data")
        .and_then(|d| d.get("issues"))
        .ok_or_else(|| TrackerError::LinearUnknownPayload(summarize(body)))?;

    let nodes = issues_obj
        .get("nodes")
        .and_then(|n| n.as_array())
        .ok_or_else(|| TrackerError::LinearUnknownPayload(summarize(body)))?;
    let issues: Vec<Issue> = nodes.iter().filter_map(normalize_issue).collect();

    let has_next_page = issues_obj
        .get("pageInfo")
        .and_then(|p| p.get("hasNextPage"))
        .and_then(|v| v.as_bool())
        .unwrap_or(false);
    let end_cursor = issues_obj
        .get("pageInfo")
        .and_then(|p| p.get("endCursor"))
        .and_then(|v| v.as_str())
        .filter(|s| !s.is_empty())
        .map(str::to_string);
    Ok((
        issues,
        PageInfo {
            has_next_page,
            end_cursor,
        },
    ))
}

fn next_cursor(p: &PageInfo) -> Result<Option<String>, TrackerError> {
    if !p.has_next_page {
        return Ok(None);
    }
    match &p.end_cursor {
        Some(c) => Ok(Some(c.clone())),
        None => Err(TrackerError::LinearMissingEndCursor),
    }
}

fn normalize_issue(node: &Value) -> Option<Issue> {
    let map = node.as_object()?;
    let id = map.get("id")?.as_str()?.to_string();
    let identifier = map.get("identifier")?.as_str()?.to_string();
    let title = map.get("title")?.as_str()?.to_string();
    let description = map
        .get("description")
        .and_then(|v| v.as_str())
        .map(str::to_string);
    let priority = map
        .get("priority")
        .and_then(|v| v.as_i64())
        .and_then(|n| i32::try_from(n).ok());
    let state = map
        .get("state")
        .and_then(|s| s.get("name"))
        .and_then(|v| v.as_str())?
        .to_string();
    let branch_name = map
        .get("branchName")
        .and_then(|v| v.as_str())
        .map(str::to_string);
    let url = map.get("url").and_then(|v| v.as_str()).map(str::to_string);
    let labels = extract_labels(map);
    let blocked_by = extract_blockers(map);
    let created_at = parse_iso8601(map.get("createdAt"));
    let updated_at = parse_iso8601(map.get("updatedAt"));
    Some(Issue {
        id,
        identifier,
        title,
        description,
        priority,
        state,
        branch_name,
        url,
        labels,
        blocked_by,
        created_at,
        updated_at,
    })
}

fn extract_labels(map: &serde_json::Map<String, Value>) -> Vec<String> {
    map.get("labels")
        .and_then(|l| l.get("nodes"))
        .and_then(|v| v.as_array())
        .map(|arr| {
            arr.iter()
                .filter_map(|n| n.get("name").and_then(|v| v.as_str()))
                .map(|s| s.to_lowercase())
                .collect()
        })
        .unwrap_or_default()
}

fn extract_blockers(map: &serde_json::Map<String, Value>) -> Vec<Blocker> {
    let nodes = match map
        .get("inverseRelations")
        .and_then(|r| r.get("nodes"))
        .and_then(|n| n.as_array())
    {
        Some(arr) => arr,
        None => return Vec::new(),
    };
    nodes
        .iter()
        .filter_map(|n| {
            let relation_type = n.get("type")?.as_str()?.trim();
            if !relation_type.eq_ignore_ascii_case("blocks") {
                return None;
            }
            let issue = n.get("issue")?.as_object()?;
            Some(Blocker {
                id: issue.get("id").and_then(|v| v.as_str()).map(str::to_string),
                identifier: issue
                    .get("identifier")
                    .and_then(|v| v.as_str())
                    .map(str::to_string),
                state: issue
                    .get("state")
                    .and_then(|s| s.get("name"))
                    .and_then(|v| v.as_str())
                    .map(str::to_string),
            })
        })
        .collect()
}

fn parse_iso8601(v: Option<&Value>) -> Option<OffsetDateTime> {
    let raw = v?.as_str()?;
    OffsetDateTime::parse(raw, &time::format_description::well_known::Iso8601::DEFAULT).ok()
}

fn summarize(value: &Value) -> String {
    let mut s = value.to_string();
    if s.len() > 200 {
        s.truncate(200);
        s.push_str("…<truncated>");
    }
    s
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;
    use std::sync::Mutex;

    /// Records every (query, variables) pair and replies with a queue of
    /// pre-canned responses.
    struct ScriptedTransport {
        calls: Mutex<Vec<(String, Value)>>,
        responses: Mutex<Vec<Result<Value, TrackerError>>>,
    }

    impl ScriptedTransport {
        fn new(responses: Vec<Result<Value, TrackerError>>) -> Arc<Self> {
            Arc::new(ScriptedTransport {
                calls: Mutex::new(Vec::new()),
                responses: Mutex::new(responses),
            })
        }

        fn calls(&self) -> Vec<(String, Value)> {
            self.calls.lock().unwrap().clone()
        }
    }

    #[async_trait]
    impl GraphqlTransport for ScriptedTransport {
        async fn post(&self, query: &str, variables: Value) -> Result<Value, TrackerError> {
            self.calls
                .lock()
                .unwrap()
                .push((query.to_string(), variables));
            let mut q = self.responses.lock().unwrap();
            assert!(!q.is_empty(), "scripted transport ran out of responses");
            q.remove(0)
        }
    }

    fn config() -> LinearConfig {
        LinearConfig {
            endpoint: "https://api.linear.app/graphql".into(),
            api_key: "token".into(),
            project_slug: "demo".into(),
            active_states: vec!["Todo".into(), "In Progress".into()],
            terminal_states: vec!["Done".into()],
        }
    }

    fn issue_node(id: &str, identifier: &str, state: &str) -> Value {
        json!({
            "id": id,
            "identifier": identifier,
            "title": format!("issue {identifier}"),
            "description": null,
            "priority": 2,
            "state": { "name": state },
            "branchName": null,
            "url": null,
            "labels": { "nodes": [{ "name": "Bug" }, { "name": "BACKEND" }] },
            "inverseRelations": {
                "nodes": [
                    {
                        "type": "blocks",
                        "issue": { "id": "B1", "identifier": "MT-99", "state": { "name": "Todo" } }
                    },
                    {
                        "type": "duplicate_of",
                        "issue": { "id": "B2", "identifier": "MT-100", "state": { "name": "Done" } }
                    }
                ]
            },
            "createdAt": "2026-01-01T00:00:00.000Z",
            "updatedAt": "2026-01-02T00:00:00.000Z"
        })
    }

    fn page_response(nodes: Value, has_next: bool, end_cursor: Option<&str>) -> Value {
        json!({
            "data": {
                "issues": {
                    "nodes": nodes,
                    "pageInfo": {
                        "hasNextPage": has_next,
                        "endCursor": end_cursor
                    }
                }
            }
        })
    }

    #[tokio::test]
    async fn fetches_candidate_issues_with_project_slug_filter() {
        let nodes = json!([issue_node("a", "MT-1", "Todo")]);
        let transport = ScriptedTransport::new(vec![Ok(page_response(nodes, false, Some("end")))]);
        let client = LinearClient::with_transport(config(), transport.clone());

        let issues = client.fetch_candidate_issues().await.unwrap();
        assert_eq!(issues.len(), 1);
        assert_eq!(issues[0].identifier, "MT-1");

        let calls = transport.calls();
        assert_eq!(calls.len(), 1);
        let (query, vars) = &calls[0];
        assert!(query.contains("slugId"), "query should filter by slugId");
        assert_eq!(vars["projectSlug"], "demo");
        assert_eq!(vars["first"], PAGE_SIZE);
        assert_eq!(
            vars["stateNames"],
            json!(["Todo", "In Progress"]),
            "active_states should be passed through"
        );
    }

    #[tokio::test]
    async fn paginates_until_has_next_page_false() {
        let p1 = page_response(json!([issue_node("a", "MT-1", "Todo")]), true, Some("c1"));
        let p2 = page_response(json!([issue_node("b", "MT-2", "Todo")]), false, None);
        let transport = ScriptedTransport::new(vec![Ok(p1), Ok(p2)]);
        let client = LinearClient::with_transport(config(), transport.clone());

        let issues = client.fetch_candidate_issues().await.unwrap();
        assert_eq!(
            issues
                .iter()
                .map(|i| i.identifier.clone())
                .collect::<Vec<_>>(),
            vec!["MT-1", "MT-2"]
        );
        let calls = transport.calls();
        assert_eq!(calls.len(), 2);
        assert!(calls[0].1["after"].is_null());
        assert_eq!(calls[1].1["after"], "c1");
    }

    #[tokio::test]
    async fn missing_end_cursor_with_has_next_is_typed_error() {
        let p = page_response(json!([issue_node("a", "MT-1", "Todo")]), true, None);
        let transport = ScriptedTransport::new(vec![Ok(p)]);
        let client = LinearClient::with_transport(config(), transport);
        let err = client.fetch_candidate_issues().await.unwrap_err();
        assert!(matches!(err, TrackerError::LinearMissingEndCursor));
    }

    #[tokio::test]
    async fn graphql_errors_become_typed_error() {
        let body = json!({ "errors": [{ "message": "unauthorized" }] });
        let transport = ScriptedTransport::new(vec![Ok(body)]);
        let client = LinearClient::with_transport(config(), transport);
        let err = client.fetch_candidate_issues().await.unwrap_err();
        match err {
            TrackerError::LinearGraphqlErrors(s) => assert!(s.contains("unauthorized")),
            other => panic!("unexpected: {other:?}"),
        }
    }

    #[tokio::test]
    async fn unknown_payload_is_typed_error() {
        let transport = ScriptedTransport::new(vec![Ok(json!({"hello": "world"}))]);
        let client = LinearClient::with_transport(config(), transport);
        let err = client.fetch_candidate_issues().await.unwrap_err();
        assert!(matches!(err, TrackerError::LinearUnknownPayload(_)));
    }

    #[tokio::test]
    async fn empty_states_list_returns_empty_without_call() {
        let transport = ScriptedTransport::new(vec![]);
        let client = LinearClient::with_transport(config(), transport.clone());
        let issues = client.fetch_issues_by_states(&[]).await.unwrap();
        assert!(issues.is_empty());
        assert!(transport.calls().is_empty());
    }

    #[tokio::test]
    async fn empty_id_list_returns_empty_without_call() {
        let transport = ScriptedTransport::new(vec![]);
        let client = LinearClient::with_transport(config(), transport.clone());
        let states = client.fetch_issue_states_by_ids(&[]).await.unwrap();
        assert!(states.is_empty());
        assert!(transport.calls().is_empty());
    }

    #[tokio::test]
    async fn refresh_by_ids_uses_id_typed_variable_and_preserves_order() {
        let nodes = json!([
            issue_node("b", "MT-2", "In Progress"),
            issue_node("a", "MT-1", "Done")
        ]);
        let body = json!({ "data": { "issues": { "nodes": nodes } } });
        let transport = ScriptedTransport::new(vec![Ok(body)]);
        let client = LinearClient::with_transport(config(), transport.clone());

        let states = client
            .fetch_issue_states_by_ids(&["a".into(), "b".into()])
            .await
            .unwrap();

        assert_eq!(
            states.iter().map(|s| s.id.clone()).collect::<Vec<_>>(),
            vec!["a", "b"],
            "results should be reordered to match the requested IDs"
        );
        let (query, vars) = &transport.calls()[0];
        assert!(
            query.contains("[ID!]!"),
            "must use ID typing per SPEC §11.2"
        );
        assert_eq!(vars["ids"], json!(["a", "b"]));
    }

    #[test]
    fn normalizes_labels_to_lowercase_and_extracts_blockers() {
        let issue = normalize_issue(&issue_node("a", "MT-1", "Todo")).unwrap();
        assert_eq!(issue.labels, vec!["bug", "backend"]);
        assert_eq!(
            issue.blocked_by.len(),
            1,
            "only `blocks` relation should count"
        );
        let blocker = &issue.blocked_by[0];
        assert_eq!(blocker.id.as_deref(), Some("B1"));
        assert_eq!(blocker.identifier.as_deref(), Some("MT-99"));
        assert_eq!(blocker.state.as_deref(), Some("Todo"));
    }

    #[test]
    fn parses_priority_only_when_integer() {
        let mut node = issue_node("a", "MT-1", "Todo");
        node["priority"] = json!("not a number");
        let issue = normalize_issue(&node).unwrap();
        assert!(issue.priority.is_none());
    }
}
