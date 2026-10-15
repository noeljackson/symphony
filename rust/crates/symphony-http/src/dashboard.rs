//! Elixir dashboard presentation port, using the existing REST/SSE contract.

use crate::api::StateView;

pub(crate) fn render_dashboard(view: Option<&StateView>) -> String {
    // JSON is inside a script data block. Escape HTML delimiters so tracker
    // messages cannot close that element, even though the block is not executable.
    let initial = serde_json::to_string(&view)
        .unwrap_or_else(|_| "null".into())
        .replace('&', "\\u0026")
        .replace('<', "\\u003c")
        .replace('>', "\\u003e")
        .replace('\u{2028}', "\\u2028")
        .replace('\u{2029}', "\\u2029");
    include_str!("dashboard/dashboard.html")
        .replace(
            "/* DASHBOARD_CSS */",
            include_str!("dashboard/dashboard.css"),
        )
        .replace("/* DASHBOARD_JS */", include_str!("dashboard/dashboard.js"))
        .replace(
            "{{ERROR_HIDDEN}}",
            if view.is_some() { "hidden" } else { "" },
        )
        .replace("{{INITIAL_STATE}}", &initial)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::api::{Counts, RetryRowView, TotalsView};

    #[test]
    fn tracker_messages_cannot_escape_the_snapshot_script() {
        let message = "</script><script>alert('injected')</script> & \u{2028}\u{2029}";
        let view = StateView {
            generated_at: "2026-10-15T12:00:00Z".into(),
            counts: Counts {
                running: 0,
                retrying: 1,
            },
            running: vec![],
            retrying: vec![RetryRowView {
                issue_id: "1".into(),
                issue_identifier: "EX-1".into(),
                attempt: 1,
                due_at: "2026-10-15T12:01:00Z".into(),
                error: Some(message.into()),
            }],
            agent_totals: TotalsView {
                input_tokens: 0,
                output_tokens: 0,
                total_tokens: 0,
                seconds_running: 0.,
                cost_usd: None,
                cost_usd_today: None,
            },
            rate_limits: None,
        };
        let html = render_dashboard(Some(&view));
        assert!(!html.contains(message));
        let data = html
            .split("<script id=\"initial-state\" type=\"application/json\">")
            .nth(1)
            .unwrap()
            .split("</script>")
            .next()
            .unwrap();
        let restored: serde_json::Value = serde_json::from_str(data).unwrap();
        assert_eq!(restored["retrying"][0]["error"], message);
    }

    #[test]
    fn unavailable_snapshot_keeps_the_dashboard_shell_and_recovery_script() {
        let html = render_dashboard(None);
        assert!(html.contains("Operations Dashboard"));
        let error_tag = html
            .split("id=\"snapshot-error\"")
            .nth(1)
            .unwrap()
            .split('>')
            .next()
            .unwrap();
        assert!(!error_tag.split_whitespace().any(|word| word == "hidden"));
        assert!(html.contains("Snapshot unavailable"));
        assert!(html.contains("new EventSource('/api/v1/events')"));
    }
}
