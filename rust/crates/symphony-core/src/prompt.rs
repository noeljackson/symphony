//! SPEC §12: Liquid prompt rendering with strict variable/filter checking.

use liquid::ParserBuilder;
use liquid_core::Object;
use serde_yaml::Value as YamlValue;

use crate::errors::PromptError;
use crate::issue::Issue;

/// Default fallback prompt body used when the workflow body is empty
/// (SPEC §5.4).
pub const DEFAULT_PROMPT_BODY: &str = "You are working on an issue from Linear.";

pub struct PromptBuilder {
    template: String,
}

impl PromptBuilder {
    pub fn new(template: &str) -> Self {
        let body = template.trim();
        let resolved = if body.is_empty() {
            DEFAULT_PROMPT_BODY.to_string()
        } else {
            body.to_string()
        };
        Self { template: resolved }
    }

    /// Render with `issue` and OPTIONAL `attempt` integer.
    pub fn render(&self, issue: &Issue, attempt: Option<u32>) -> Result<String, PromptError> {
        let parser = ParserBuilder::with_stdlib()
            .build()
            .map_err(|e| PromptError::Parse(e.to_string()))?;
        let template = parser
            .parse(&self.template)
            .map_err(|e| PromptError::Parse(e.to_string()))?;

        let mut globals = Object::new();
        globals.insert("issue".into(), issue_to_liquid(issue));
        let attempt_val = match attempt {
            Some(n) => liquid_core::Value::scalar(n as i64),
            None => liquid_core::Value::Nil,
        };
        globals.insert("attempt".into(), attempt_val);

        template
            .render(&globals)
            .map_err(|e| PromptError::Render(e.to_string()))
    }
}

fn issue_to_liquid(issue: &Issue) -> liquid_core::Value {
    let json = serde_json::to_value(issue).unwrap_or(serde_json::Value::Null);
    json_to_liquid(&json)
}

fn json_to_liquid(v: &serde_json::Value) -> liquid_core::Value {
    use liquid_core::Value;
    match v {
        serde_json::Value::Null => Value::Nil,
        serde_json::Value::Bool(b) => Value::scalar(*b),
        serde_json::Value::Number(n) => {
            if let Some(i) = n.as_i64() {
                Value::scalar(i)
            } else if let Some(f) = n.as_f64() {
                Value::scalar(f)
            } else {
                Value::scalar(n.to_string())
            }
        }
        serde_json::Value::String(s) => Value::scalar(s.clone()),
        serde_json::Value::Array(items) => {
            let mut out = Vec::with_capacity(items.len());
            for item in items {
                out.push(json_to_liquid(item));
            }
            Value::Array(out)
        }
        serde_json::Value::Object(map) => {
            let mut object = Object::new();
            for (k, val) in map {
                object.insert(k.clone().into(), json_to_liquid(val));
            }
            Value::Object(object)
        }
    }
}

/// Convenience converter used by extensions that store config snippets as YAML
/// values.
pub fn yaml_to_liquid(v: &YamlValue) -> liquid_core::Value {
    use liquid_core::Value;
    match v {
        YamlValue::Null => Value::Nil,
        YamlValue::Bool(b) => Value::scalar(*b),
        YamlValue::Number(n) => {
            if let Some(i) = n.as_i64() {
                Value::scalar(i)
            } else if let Some(f) = n.as_f64() {
                Value::scalar(f)
            } else {
                Value::scalar(n.to_string())
            }
        }
        YamlValue::String(s) => Value::scalar(s.clone()),
        YamlValue::Sequence(items) => Value::Array(items.iter().map(yaml_to_liquid).collect()),
        YamlValue::Mapping(map) => {
            let mut object = Object::new();
            for (k, val) in map {
                if let Some(key) = k.as_str() {
                    object.insert(key.to_string().into(), yaml_to_liquid(val));
                }
            }
            Value::Object(object)
        }
        YamlValue::Tagged(t) => yaml_to_liquid(&t.value),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::issue::{Blocker, Issue};

    fn sample() -> Issue {
        Issue {
            id: "id-1".into(),
            identifier: "MT-1".into(),
            title: "do thing".into(),
            description: Some("desc".into()),
            priority: Some(2),
            state: "Todo".into(),
            branch_name: None,
            url: None,
            labels: vec!["bug".into()],
            blocked_by: vec![Blocker {
                id: Some("x".into()),
                identifier: Some("MT-2".into()),
                state: Some("Done".into()),
            }],
            created_at: None,
            updated_at: None,
        }
    }

    #[test]
    fn renders_issue_fields_and_attempt() {
        let p = PromptBuilder::new(
            "issue {{ issue.identifier }} ({{ issue.state }}) attempt={{ attempt }}",
        );
        let out = p.render(&sample(), Some(2)).unwrap();
        assert_eq!(out, "issue MT-1 (Todo) attempt=2");
    }

    #[test]
    fn iterates_over_labels_and_blockers() {
        let p = PromptBuilder::new(
            "labels:{% for l in issue.labels %} {{ l }}{% endfor %} blockers:{% for b in issue.blocked_by %} {{ b.identifier }}={{ b.state }}{% endfor %}",
        );
        let out = p.render(&sample(), None).unwrap();
        assert!(out.contains("labels: bug"));
        assert!(out.contains("blockers: MT-2=Done"));
    }

    #[test]
    fn unknown_filter_is_render_error() {
        let p = PromptBuilder::new("{{ issue.title | nope }}");
        let err = p.render(&sample(), None).unwrap_err();
        assert!(matches!(
            err,
            PromptError::Parse(_) | PromptError::Render(_)
        ));
    }

    #[test]
    fn empty_body_falls_back_to_default() {
        let p = PromptBuilder::new("");
        let out = p.render(&sample(), None).unwrap();
        assert_eq!(out, DEFAULT_PROMPT_BODY);
    }
}
