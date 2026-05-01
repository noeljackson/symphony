//! `symphony logs <identifier>` subcommand.
//!
//! SPEC v2 §13.7.2 + §18.2 contract:
//!
//! 1. GET `<url>/api/v1/<identifier>` for backfill (`recent_events`).
//! 2. With `--follow` (default) subscribe to `<url>/api/v1/events` and
//!    print events whose `issue_identifier` matches.
//! 3. With `--no-follow`, print backfill and exit `0`.
//! 4. 404 + empty backfill → exit `1` ("issue not tracked").
//!
//! The output is one line per event keyed off `at`, `event`, `message`.
//! When the per-issue response includes `agent_session_logs`, the first
//! entry's `path` is surfaced on stderr as a hint per SPEC §18.2.

use std::io::Write;

use anyhow::{anyhow, Context};
use futures::StreamExt;
use reqwest::StatusCode;

const SSE_DEFAULT_TIMEOUT_SECS: u64 = 30;

/// One row of `/api/v1/<id>.recent_events`.
#[derive(Debug, serde::Deserialize)]
struct RecentEvent {
    at: String,
    event: String,
    #[serde(default)]
    message: Option<String>,
}

/// Subset of `/api/v1/<id>` we care about for log tailing.
#[derive(Debug, serde::Deserialize)]
struct IssueResponse {
    #[serde(default)]
    recent_events: Vec<RecentEvent>,
    #[serde(default)]
    logs: Option<LogsBlock>,
}

#[derive(Debug, serde::Deserialize)]
struct LogsBlock {
    #[serde(default)]
    agent_session_logs: Vec<LogRef>,
}

#[derive(Debug, serde::Deserialize)]
struct LogRef {
    #[allow(dead_code)]
    label: Option<String>,
    path: Option<String>,
}

/// Subset of an SSE `data:` payload we care about.
#[derive(Debug, serde::Deserialize)]
struct StreamedEvent {
    issue_identifier: Option<String>,
    timestamp: Option<String>,
    event: Option<String>,
    #[serde(default)]
    message: Option<String>,
}

pub struct LogsArgs<'a> {
    pub identifier: &'a str,
    pub url: &'a str,
    pub follow: bool,
}

/// Run the `symphony logs` flow against `args.url`. Writes one line per
/// event to `out`. Returns the process exit code (`0` success / `1`
/// "issue not tracked" / `2` connection / parse error).
pub async fn run<W: Write + Send>(args: LogsArgs<'_>, mut out: W) -> anyhow::Result<u8> {
    let base = args.url.trim_end_matches('/');
    let issue_url = format!("{base}/api/v1/{}", args.identifier);

    let client = reqwest::Client::builder()
        .build()
        .context("build http client")?;

    let resp = client
        .get(&issue_url)
        .send()
        .await
        .with_context(|| format!("connect to {issue_url}"))?;

    match resp.status() {
        StatusCode::OK => {}
        StatusCode::NOT_FOUND => {
            eprintln!("symphony logs: issue not tracked: {}", args.identifier);
            return Ok(1);
        }
        other => {
            eprintln!(
                "symphony logs: unexpected status {} from {}",
                other.as_u16(),
                issue_url
            );
            return Ok(2);
        }
    }

    let body: IssueResponse = resp
        .json()
        .await
        .with_context(|| format!("parse JSON from {issue_url}"))?;

    if let Some(logs) = &body.logs {
        if let Some(first) = logs.agent_session_logs.first() {
            if let Some(p) = first.path.as_deref() {
                eprintln!("symphony logs: agent_session_logs[0].path = {p}");
            }
        }
    }

    for ev in &body.recent_events {
        write_event_line(&mut out, &ev.at, &ev.event, ev.message.as_deref())?;
    }

    if !args.follow {
        return Ok(0);
    }

    let stream_url = format!("{base}/api/v1/events");
    let resp = client
        .get(&stream_url)
        .timeout(std::time::Duration::from_secs(SSE_DEFAULT_TIMEOUT_SECS))
        .send()
        .await;
    let resp = match resp {
        Ok(r) if r.status().is_success() => r,
        Ok(r) => {
            eprintln!(
                "symphony logs: SSE failed: status {} from {}",
                r.status().as_u16(),
                stream_url
            );
            // We already printed backfill — leave that visible.
            return Ok(0);
        }
        Err(e) => {
            eprintln!("symphony logs: SSE failed: {e}");
            return Ok(0);
        }
    };

    let want = args.identifier.to_string();
    let mut stream = resp.bytes_stream();
    let mut buf = String::new();
    let mut current_event = String::new();
    let mut current_data = String::new();

    while let Some(chunk) = stream.next().await {
        let chunk = match chunk {
            Ok(c) => c,
            Err(e) => {
                eprintln!("symphony logs: SSE stream error: {e}");
                return Ok(0);
            }
        };
        buf.push_str(&String::from_utf8_lossy(&chunk));
        // SSE messages are separated by a blank line (`\n\n`).
        while let Some(term) = buf.find("\n\n") {
            let raw_msg = buf[..term].to_string();
            buf.drain(..(term + 2));

            current_event.clear();
            current_data.clear();
            for line in raw_msg.split('\n') {
                if let Some(rest) = line.strip_prefix("event:") {
                    current_event.push_str(rest.trim());
                } else if let Some(rest) = line.strip_prefix("data:") {
                    if !current_data.is_empty() {
                        current_data.push('\n');
                    }
                    current_data.push_str(rest.trim());
                }
            }
            if current_event == "snapshot" || current_event == "lagged" {
                continue;
            }
            if current_data.is_empty() {
                continue;
            }
            let parsed: Result<StreamedEvent, _> = serde_json::from_str(&current_data);
            let parsed = match parsed {
                Ok(p) => p,
                Err(_) => continue,
            };
            if parsed.issue_identifier.as_deref() != Some(want.as_str()) {
                continue;
            }
            let at = parsed.timestamp.as_deref().unwrap_or("");
            let kind = parsed.event.as_deref().unwrap_or(&current_event);
            write_event_line(&mut out, at, kind, parsed.message.as_deref())?;
        }
    }
    Ok(0)
}

fn write_event_line<W: Write>(
    out: &mut W,
    at: &str,
    event: &str,
    message: Option<&str>,
) -> std::io::Result<()> {
    match message {
        Some(m) if !m.is_empty() => writeln!(out, "{at}  {event}  {m}"),
        _ => writeln!(out, "{at}  {event}"),
    }
}

pub fn parse_url(url: &str) -> anyhow::Result<&str> {
    if url.starts_with("http://") || url.starts_with("https://") {
        Ok(url)
    } else {
        Err(anyhow!("--url must start with http:// or https://"))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_url_accepts_http_and_https() {
        assert!(parse_url("http://localhost:8080").is_ok());
        assert!(parse_url("https://example.test").is_ok());
    }

    #[test]
    fn parse_url_rejects_other_schemes() {
        assert!(parse_url("ftp://x").is_err());
        assert!(parse_url("localhost:8080").is_err());
    }

    #[test]
    fn write_event_line_includes_message_when_present() {
        let mut out = Vec::new();
        write_event_line(&mut out, "2026-05-01T00:00:00Z", "notification", Some("hi")).unwrap();
        assert_eq!(
            String::from_utf8(out).unwrap(),
            "2026-05-01T00:00:00Z  notification  hi\n"
        );
    }

    #[test]
    fn write_event_line_omits_message_when_empty() {
        let mut out = Vec::new();
        write_event_line(&mut out, "t", "session_started", None).unwrap();
        write_event_line(&mut out, "t", "session_started", Some("")).unwrap();
        assert_eq!(
            String::from_utf8(out).unwrap(),
            "t  session_started\nt  session_started\n"
        );
    }
}
