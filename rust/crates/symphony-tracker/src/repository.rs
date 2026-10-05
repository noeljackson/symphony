//! GitHub and Forgejo repository readers, SPEC v3 §5.3.1.B/C and §11.

use std::collections::HashSet;

use async_trait::async_trait;
use base64::{
    engine::general_purpose::{STANDARD, URL_SAFE_NO_PAD},
    Engine,
};
use reqwest::{
    header::{HeaderValue, AUTHORIZATION, LINK},
    Client, Response, Url,
};
use ring::{
    rand::SystemRandom,
    signature::{RsaKeyPair, RSA_PKCS1_SHA256},
};
use serde::Deserialize;
use serde_json::{json, Value};
use symphony_core::{
    config::{RepositoryConfig, TrackerConfig, TrackerKind},
    Issue,
};
use time::{format_description::well_known::Rfc3339, OffsetDateTime};
use tokio::sync::Mutex;

use crate::{IssueState, Tracker, TrackerError};

pub struct RepositoryClient {
    kind: &'static str,
    http: Client,
    endpoint: Url,
    config: RepositoryConfig,
    active_states: Vec<String>,
    terminal_states: Vec<String>,
    identity: String,
    auth: Auth,
}

enum Auth {
    Token(String),
    App {
        key: Box<RsaKeyPair>,
        app_id: String,
        installation_id: u64,
        cached: Mutex<Option<InstallationToken>>,
    },
}

struct InstallationToken {
    value: String,
    expires_at: OffsetDateTime,
}

#[derive(Deserialize)]
struct ApiIssue {
    number: u64,
    title: String,
    body: Option<String>,
    state: String,
    html_url: Option<String>,
    #[serde(default)]
    labels: Option<Vec<ApiLabel>>,
    #[serde(default)]
    assignees: Option<Vec<ApiUser>>,
    created_at: Option<String>,
    updated_at: Option<String>,
    pull_request: Option<Value>,
}

#[derive(Deserialize)]
#[serde(untagged)]
enum ApiLabel {
    Name(String),
    Object { name: String },
}

#[derive(Deserialize)]
struct ApiUser {
    login: String,
}

impl RepositoryClient {
    pub fn new(tracker: &TrackerConfig) -> Result<Self, TrackerError> {
        let kind = match &tracker.kind {
            TrackerKind::Github => "github",
            TrackerKind::Forgejo => "forgejo",
            _ => {
                return Err(TrackerError::UnsupportedTrackerKind(
                    "expected github or forgejo".into(),
                ))
            }
        };
        let config = tracker.repository.clone().ok_or_else(|| {
            TrackerError::RepositoryConfig(kind, "missing selected repository configuration".into())
        })?;
        config
            .validate(&tracker.kind)
            .map_err(|e| TrackerError::RepositoryConfig(kind, e.to_string()))?;
        if tracker.terminal_states.is_empty() {
            return Err(TrackerError::RepositoryConfig(
                kind,
                "terminal_states must contain a state for closed issues".into(),
            ));
        }
        let mut endpoint = Url::parse(&tracker.endpoint).map_err(|_| {
            TrackerError::RepositoryConfig(kind, "endpoint must be an HTTP(S) API base URL".into())
        })?;
        if !matches!(endpoint.scheme(), "http" | "https")
            || endpoint.host_str().is_none()
            || !endpoint.username().is_empty()
            || endpoint.password().is_some()
            || endpoint.query().is_some()
            || endpoint.fragment().is_some()
        {
            return Err(TrackerError::RepositoryConfig(
                kind,
                "endpoint must be HTTP(S) without credentials, query, or fragment".into(),
            ));
        }
        if kind == "forgejo" && !endpoint.path().trim_end_matches('/').ends_with("/api/v1") {
            return Err(TrackerError::RepositoryConfig(
                kind,
                "Forgejo endpoint must include /api/v1".into(),
            ));
        }
        let path = format!("{}/", endpoint.path().trim_end_matches('/'));
        endpoint.set_path(&path);
        let identity = format!(
            "{kind}:{}repos/{}/{}/issues/",
            endpoint, config.owner, config.repo
        );
        let auth = if let Some(app_id) = config.app_id.as_ref().filter(|_| kind == "github") {
            Auth::App {
                key: Box::new(parse_key(
                    config.private_key.as_deref().unwrap_or_default(),
                )?),
                app_id: app_id.clone(),
                installation_id: config
                    .app_installation_id
                    .as_deref()
                    .unwrap_or_default()
                    .parse()
                    .map_err(|_| TrackerError::GithubAuth("invalid installation ID"))?,
                cached: Mutex::new(None),
            }
        } else {
            Auth::Token(config.api_token.clone().unwrap_or_default())
        };
        // Redirects never carry credentials to another service or login page.
        let http = Client::builder()
            .timeout(std::time::Duration::from_secs(30))
            .redirect(reqwest::redirect::Policy::none())
            .user_agent("symphony/0.1")
            .build()
            .map_err(|_| TrackerError::RepositoryApiRequest(kind))?;
        Ok(Self {
            kind,
            http,
            endpoint,
            config,
            active_states: tracker.active_states.clone(),
            terminal_states: tracker.terminal_states.clone(),
            identity,
            auth,
        })
    }

    fn url(&self, segments: &[&str]) -> Result<Url, TrackerError> {
        let mut url = self.endpoint.clone();
        url.path_segments_mut()
            .map_err(|_| TrackerError::RepositoryConfig(self.kind, "invalid API base URL".into()))?
            .pop_if_empty()
            .extend(segments);
        Ok(url)
    }

    fn issues_url(&self) -> Result<Url, TrackerError> {
        self.url(&["repos", &self.config.owner, &self.config.repo, "issues"])
    }

    async fn token(&self) -> Result<String, TrackerError> {
        match &self.auth {
            Auth::Token(token) => Ok(token.clone()),
            Auth::App {
                key,
                app_id,
                installation_id,
                cached,
            } => {
                // Serialize exchanges so concurrent polling/reconciliation share a token.
                let mut cache = cached.lock().await;
                let now = OffsetDateTime::now_utc();
                if let Some(token) = cache.as_ref() {
                    if token.expires_at > now + time::Duration::seconds(60) {
                        return Ok(token.value.clone());
                    }
                }
                let jwt = jwt(key, app_id, now)?;
                let url = self.url(&[
                    "app",
                    "installations",
                    &installation_id.to_string(),
                    "access_tokens",
                ])?;
                let response = self.http.post(url)
                    .header(AUTHORIZATION, auth_header("github", &jwt)?)
                    .header("Accept", "application/vnd.repository+json")
                    .header("X-GitHub-Api-Version", "2022-11-28")
                    .json(&json!({"repositories": [self.config.repo], "permissions": {"issues": "read"}}))
                    .send().await.map_err(|_| TrackerError::RepositoryApiRequest(self.kind))?;
                if !response.status().is_success() {
                    return Err(TrackerError::RepositoryApiStatus(
                        self.kind,
                        response.status().as_u16(),
                    ));
                }
                #[derive(Deserialize)]
                struct Exchange {
                    token: String,
                    expires_at: String,
                }
                let exchange: Exchange = response
                    .json()
                    .await
                    .map_err(|_| TrackerError::GithubAuth("invalid installation token response"))?;
                let expires_at = OffsetDateTime::parse(&exchange.expires_at, &Rfc3339)
                    .map_err(|_| TrackerError::GithubAuth("invalid installation token expiry"))?;
                if exchange.token.is_empty() || expires_at <= now + time::Duration::seconds(60) {
                    return Err(TrackerError::GithubAuth(
                        "empty or expired installation token",
                    ));
                }
                let value = exchange.token;
                *cache = Some(InstallationToken {
                    value: value.clone(),
                    expires_at,
                });
                Ok(value)
            }
        }
    }

    async fn get(&self, url: Url) -> Result<Response, TrackerError> {
        let mut retried = false;
        loop {
            let token = self.token().await?;
            let mut request = self
                .http
                .get(url.clone())
                .header(AUTHORIZATION, auth_header(self.kind, &token)?)
                .header(
                    "Accept",
                    if self.kind == "github" {
                        "application/vnd.github+json"
                    } else {
                        "application/json"
                    },
                );
            if self.kind == "github" {
                request = request.header("X-GitHub-Api-Version", "2022-11-28");
            }
            let response = request
                .send()
                .await
                .map_err(|_| TrackerError::RepositoryApiRequest(self.kind))?;
            if response.status().as_u16() == 401 && !retried {
                if let Auth::App { cached, .. } = &self.auth {
                    let mut cache = cached.lock().await;
                    if cache.as_ref().is_some_and(|t| t.value == token) {
                        *cache = None;
                    }
                    retried = true;
                    continue;
                }
            }
            return Ok(response);
        }
    }

    async fn list(&self) -> Result<Vec<(Issue, Vec<String>)>, TrackerError> {
        let mut page = 1u64;
        let mut seen = HashSet::new();
        let mut result = Vec::new();
        loop {
            let mut url = self.issues_url()?;
            let page_size = if self.kind == "github" { 100 } else { 50 };
            {
                let mut query = url.query_pairs_mut();
                query.append_pair("state", "all");
                if self.kind == "github" {
                    query
                        .append_pair("sort", "created")
                        .append_pair("direction", "asc")
                        .append_pair("per_page", "100");
                } else {
                    query
                        .append_pair("type", "issues")
                        .append_pair("sort", "oldest")
                        .append_pair("limit", "50");
                }
                query.append_pair("page", &page.to_string());
            }
            let response = self.get(url).await?;
            if response.status().as_u16() != 200 {
                return Err(TrackerError::RepositoryApiStatus(
                    self.kind,
                    response.status().as_u16(),
                ));
            }
            let has_link = response.headers().contains_key(LINK);
            let next_page = self.next_page(response.headers(), page)?;
            let issues: Vec<ApiIssue> = response.json().await.map_err(|_| {
                TrackerError::RepositoryUnknownPayload(self.kind, "expected issue array")
            })?;
            let count = issues.len();
            let mut new_numbers = 0;
            for issue in issues {
                if !seen.insert(issue.number) {
                    continue;
                }
                new_numbers += 1;
                if issue.pull_request.is_some() {
                    continue;
                }
                let assignees = issue
                    .assignees
                    .iter()
                    .flatten()
                    .map(|u| u.login.clone())
                    .collect();
                result.push((self.normalize(issue)?, assignees));
            }
            if next_page.is_none() && (has_link || count < page_size) {
                break;
            }
            if new_numbers == 0 {
                return Err(TrackerError::RepositoryUnknownPayload(
                    self.kind,
                    "pagination did not advance",
                ));
            }
            page = match next_page {
                Some(next) => next,
                None => page
                    .checked_add(1)
                    .ok_or(TrackerError::RepositoryUnknownPayload(
                        self.kind,
                        "pagination overflow",
                    ))?,
            };
        }
        Ok(result)
    }

    fn next_page(
        &self,
        headers: &reqwest::header::HeaderMap,
        current: u64,
    ) -> Result<Option<u64>, TrackerError> {
        let invalid =
            || TrackerError::RepositoryUnknownPayload(self.kind, "invalid pagination link");
        let mut next = None;
        for value in headers.get_all(LINK) {
            for entry in value.to_str().map_err(|_| invalid())?.split(',') {
                let mut parts = entry.split(';');
                let target = parts
                    .next()
                    .ok_or_else(invalid)?
                    .trim()
                    .strip_prefix('<')
                    .and_then(|s| s.strip_suffix('>'))
                    .ok_or_else(invalid)?;
                if !parts.any(|p| p.trim() == "rel=\"next\"") {
                    continue;
                }
                let url = self.endpoint.join(target).map_err(|_| invalid())?;
                let page = url
                    .query_pairs()
                    .find(|(key, _)| key == "page")
                    .and_then(|(_, value)| value.parse::<u64>().ok())
                    .filter(|page| *page > current)
                    .ok_or_else(invalid)?;
                if next.replace(page).is_some() {
                    return Err(invalid());
                }
            }
        }
        // Only the page number is used; links never change the API host or path.
        Ok(next)
    }

    fn normalize(&self, raw: ApiIssue) -> Result<Issue, TrackerError> {
        if raw.number == 0 || !matches!(raw.state.as_str(), "open" | "closed") {
            return Err(TrackerError::RepositoryUnknownPayload(
                self.kind,
                "invalid issue number or state",
            ));
        }
        let labels: Vec<String> = raw
            .labels
            .unwrap_or_default()
            .into_iter()
            .map(|l| match l {
                ApiLabel::Name(name) | ApiLabel::Object { name } => name.to_lowercase(),
            })
            .collect();
        let state = if raw.state == "closed" {
            self.terminal_states
                .iter()
                .find(|s| s.eq_ignore_ascii_case("closed"))
                .unwrap_or(&self.terminal_states[0])
                .clone()
        } else {
            self.terminal_states
                .iter()
                .chain(self.active_states.iter())
                .find(|state| labels.iter().any(|l| l == &state.to_lowercase()))
                .cloned()
                .unwrap_or_else(|| "open".into())
        };
        let priority = labels
            .iter()
            .filter_map(|l| self.config.label_priority_map.get(l))
            .min()
            .copied()
            .unwrap_or(1);
        let slug = raw
            .title
            .to_lowercase()
            .split(|c: char| !c.is_ascii_alphanumeric())
            .filter(|s| !s.is_empty())
            .collect::<Vec<_>>()
            .join("-");
        let timestamp = |s: Option<String>| -> Result<Option<OffsetDateTime>, TrackerError> {
            s.map(|s| {
                OffsetDateTime::parse(&s, &Rfc3339).map_err(|_| {
                    TrackerError::RepositoryUnknownPayload(self.kind, "invalid issue timestamp")
                })
            })
            .transpose()
        };
        Ok(Issue {
            id: format!("{}{}", self.identity, raw.number),
            identifier: format!("{}/{}#{}", self.config.owner, self.config.repo, raw.number),
            title: raw.title,
            description: raw.body,
            priority: Some(priority),
            state,
            branch_name: Some(if slug.is_empty() {
                raw.number.to_string()
            } else {
                format!("{}-{}", raw.number, slug)
            }),
            url: raw.html_url,
            labels,
            blocked_by: Vec::new(),
            created_at: timestamp(raw.created_at)?,
            updated_at: timestamp(raw.updated_at)?,
        })
    }
}

#[async_trait]
impl Tracker for RepositoryClient {
    async fn fetch_candidate_issues(&self) -> Result<Vec<Issue>, TrackerError> {
        if self.active_states.is_empty() {
            return Ok(Vec::new());
        }
        Ok(self
            .list()
            .await?
            .into_iter()
            .filter(|(issue, assignees)| {
                self.active_states
                    .iter()
                    .any(|s| s.to_lowercase() == issue.state.to_lowercase())
                    && !self
                        .terminal_states
                        .iter()
                        .any(|s| s.to_lowercase() == issue.state.to_lowercase())
                    && self.config.assignee.as_ref().map_or(true, |a| {
                        assignees
                            .iter()
                            .any(|s| s.to_lowercase() == a.to_lowercase())
                    })
            })
            .map(|(issue, _)| issue)
            .collect())
    }

    async fn fetch_issues_by_states(&self, states: &[String]) -> Result<Vec<Issue>, TrackerError> {
        if states.is_empty() {
            return Ok(Vec::new());
        }
        Ok(self
            .list()
            .await?
            .into_iter()
            .map(|(issue, _)| issue)
            .filter(|issue| {
                states
                    .iter()
                    .any(|s| s.to_lowercase() == issue.state.to_lowercase())
            })
            .collect())
    }

    async fn fetch_issue_states_by_ids(
        &self,
        ids: &[String],
    ) -> Result<Vec<IssueState>, TrackerError> {
        // Validate the whole batch before requests; foreign-repository IDs are errors.
        let numbers: Vec<u64> = ids
            .iter()
            .map(|id| {
                id.strip_prefix(&self.identity)
                    .and_then(|n| n.parse::<u64>().ok())
                    .filter(|n| *n > 0)
                    .ok_or(TrackerError::RepositoryUnknownPayload(
                        self.kind,
                        "issue ID does not belong to the configured repository",
                    ))
            })
            .collect::<Result<_, _>>()?;
        let mut result = Vec::new();
        let mut seen = HashSet::new();
        for number in numbers {
            if !seen.insert(number) {
                continue;
            }
            let url = self.url(&[
                "repos",
                &self.config.owner,
                &self.config.repo,
                "issues",
                &number.to_string(),
            ])?;
            let response = self.get(url).await?;
            if response.status().as_u16() == 404 {
                continue;
            }
            if response.status().as_u16() != 200 {
                return Err(TrackerError::RepositoryApiStatus(
                    self.kind,
                    response.status().as_u16(),
                ));
            }
            let raw: ApiIssue = response.json().await.map_err(|_| {
                TrackerError::RepositoryUnknownPayload(self.kind, "expected issue object")
            })?;
            if raw.number != number {
                return Err(TrackerError::RepositoryUnknownPayload(
                    self.kind,
                    "issue number mismatch",
                ));
            }
            if raw.pull_request.is_some() {
                continue;
            }
            let issue = self.normalize(raw)?;
            result.push(IssueState {
                id: issue.id,
                identifier: issue.identifier,
                state: issue.state,
            });
        }
        Ok(result)
    }
}

fn auth_header(kind: &'static str, token: &str) -> Result<HeaderValue, TrackerError> {
    let mut header = HeaderValue::from_str(&format!(
        "{} {token}",
        if kind == "forgejo" { "token" } else { "Bearer" }
    ))
    .map_err(|_| TrackerError::RepositoryAuth(kind, "invalid authorization header"))?;
    header.set_sensitive(true);
    Ok(header)
}

fn parse_key(pem: &str) -> Result<RsaKeyPair, TrackerError> {
    let pkcs8 = pem.trim_start().starts_with("-----BEGIN PRIVATE KEY-----");
    let label = if pkcs8 {
        "PRIVATE KEY"
    } else {
        "RSA PRIVATE KEY"
    };
    let begin = format!("-----BEGIN {label}-----");
    let end = format!("-----END {label}-----");
    let encoded = pem
        .trim()
        .strip_prefix(begin.as_str())
        .and_then(|s| s.strip_suffix(end.as_str()))
        .ok_or(TrackerError::GithubAuth(
            "expected an RSA private key in PEM format",
        ))?;
    let der = STANDARD
        .decode(
            encoded
                .chars()
                .filter(|c| !c.is_whitespace())
                .collect::<String>(),
        )
        .map_err(|_| TrackerError::GithubAuth("invalid private key encoding"))?;
    let key = if pkcs8 {
        RsaKeyPair::from_pkcs8(&der)
    } else {
        RsaKeyPair::from_der(&der)
    };
    key.map_err(|_| TrackerError::GithubAuth("invalid RSA private key"))
}

fn jwt(key: &RsaKeyPair, app_id: &str, now: OffsetDateTime) -> Result<String, TrackerError> {
    let header = URL_SAFE_NO_PAD.encode(br#"{"alg":"RS256","typ":"JWT"}"#);
    let claims = URL_SAFE_NO_PAD.encode(
        json!({
            "iat": now.unix_timestamp() - 60, "exp": now.unix_timestamp() + 540, "iss": app_id
        })
        .to_string(),
    );
    let message = format!("{header}.{claims}");
    let mut signature = vec![0; key.public().modulus_len()];
    key.sign(
        &RSA_PKCS1_SHA256,
        &SystemRandom::new(),
        message.as_bytes(),
        &mut signature,
    )
    .map_err(|_| TrackerError::GithubAuth("JWT signing failed"))?;
    Ok(format!("{message}.{}", URL_SAFE_NO_PAD.encode(signature)))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use std::process::{Command, Stdio};

    #[test]
    fn signs_verifiable_jwt_with_pkcs8_and_pkcs1_keys() {
        let generated = Command::new("openssl")
            .args([
                "genpkey",
                "-algorithm",
                "RSA",
                "-pkeyopt",
                "rsa_keygen_bits:2048",
            ])
            .output()
            .expect("JWT test requires OpenSSL");
        assert!(generated.status.success());
        let mut conversion = Command::new("openssl")
            .args(["rsa", "-traditional"])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        conversion
            .stdin
            .take()
            .unwrap()
            .write_all(&generated.stdout)
            .unwrap();
        let pkcs1 = conversion.wait_with_output().unwrap();
        assert!(pkcs1.status.success());
        for pem in [generated.stdout, pkcs1.stdout] {
            let key = parse_key(std::str::from_utf8(&pem).unwrap()).unwrap();
            let token = jwt(&key, "123", OffsetDateTime::now_utc()).unwrap();
            let (message, encoded) = token.rsplit_once('.').unwrap();
            ring::signature::UnparsedPublicKey::new(
                &ring::signature::RSA_PKCS1_2048_8192_SHA256,
                key.public().as_ref(),
            )
            .verify(
                message.as_bytes(),
                &URL_SAFE_NO_PAD.decode(encoded).unwrap(),
            )
            .unwrap();
        }
    }
}
