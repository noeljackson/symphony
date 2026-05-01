//! SPEC §4.1.3 / §6: typed view of the workflow config.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use serde_yaml::{Mapping, Value};

use crate::errors::ConfigError;
use crate::workflow::WorkflowDefinition;

/// Tracker kinds supported by core conformance.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum TrackerKind {
    Linear,
    Other(String),
}

impl TrackerKind {
    pub fn parse(raw: &str) -> Self {
        match raw.to_lowercase().as_str() {
            "linear" => Self::Linear,
            other => Self::Other(other.to_string()),
        }
    }
}

#[derive(Debug, Clone)]
pub struct TrackerConfig {
    pub kind: TrackerKind,
    pub endpoint: String,
    pub api_key: Option<String>,
    pub project_slug: Option<String>,
    pub active_states: Vec<String>,
    pub terminal_states: Vec<String>,
}

#[derive(Debug, Clone)]
pub struct PollingConfig {
    pub interval_ms: u64,
}

#[derive(Debug, Clone)]
pub struct WorkspaceConfig {
    pub root: PathBuf,
}

#[derive(Debug, Clone, Default)]
pub struct HooksConfig {
    pub after_create: Option<String>,
    pub before_run: Option<String>,
    pub after_run: Option<String>,
    pub before_remove: Option<String>,
    pub timeout_ms: u64,
}

#[derive(Debug, Clone)]
pub struct AgentConfig {
    pub max_concurrent_agents: usize,
    pub max_turns: u32,
    pub max_retry_backoff_ms: u64,
    /// Keys are normalized to lowercase (SPEC §5.3.5).
    pub max_concurrent_agents_by_state: BTreeMap<String, usize>,
}

#[derive(Debug, Clone)]
pub struct CodexConfig {
    pub command: String,
    pub approval_policy: Option<Value>,
    pub thread_sandbox: Option<Value>,
    pub turn_sandbox_policy: Option<Value>,
    pub turn_timeout_ms: u64,
    pub read_timeout_ms: u64,
    pub stall_timeout_ms: i64,
}

#[derive(Debug, Clone)]
pub struct ServerConfig {
    /// `Some(0)` means ephemeral.
    pub port: Option<u16>,
}

#[derive(Debug, Clone)]
pub struct ServiceConfig {
    pub tracker: TrackerConfig,
    pub polling: PollingConfig,
    pub workspace: WorkspaceConfig,
    pub hooks: HooksConfig,
    pub agent: AgentConfig,
    pub codex: CodexConfig,
    pub server: ServerConfig,
    /// Original front-matter map for forward-compatibility / extensions.
    pub raw: Mapping,
    /// Path the workflow was loaded from. Used to resolve relative paths.
    pub workflow_path: PathBuf,
}

impl ServiceConfig {
    pub fn from_workflow(def: &WorkflowDefinition) -> Result<Self, ConfigError> {
        let raw = def.config.clone();
        let workflow_dir = def
            .path
            .parent()
            .map(Path::to_path_buf)
            .unwrap_or_else(|| PathBuf::from("."));

        let tracker = parse_tracker(get_map(&raw, "tracker"))?;
        let polling = parse_polling(get_map(&raw, "polling"))?;
        let workspace = parse_workspace(get_map(&raw, "workspace"), &workflow_dir)?;
        let hooks = parse_hooks(get_map(&raw, "hooks"))?;
        let agent = parse_agent(get_map(&raw, "agent"))?;
        let codex = parse_codex(get_map(&raw, "codex"))?;
        let server = parse_server(get_map(&raw, "server"))?;

        Ok(ServiceConfig {
            tracker,
            polling,
            workspace,
            hooks,
            agent,
            codex,
            server,
            raw,
            workflow_path: def.path.clone(),
        })
    }

    /// SPEC §6.3 dispatch preflight validation.
    pub fn validate_for_dispatch(&self) -> Result<(), ConfigError> {
        match &self.tracker.kind {
            TrackerKind::Linear => {
                if self.tracker.api_key.as_deref().unwrap_or("").is_empty() {
                    return Err(ConfigError::MissingTrackerApiKey);
                }
                if self
                    .tracker
                    .project_slug
                    .as_deref()
                    .unwrap_or("")
                    .is_empty()
                {
                    return Err(ConfigError::MissingTrackerProjectSlug);
                }
            }
            TrackerKind::Other(kind) => {
                return Err(ConfigError::UnsupportedTrackerKind(kind.clone()));
            }
        }
        if self.codex.command.trim().is_empty() {
            return Err(ConfigError::EmptyCodexCommand);
        }
        Ok(())
    }
}

fn get_map<'a>(raw: &'a Mapping, key: &str) -> Option<&'a Mapping> {
    raw.get(Value::from(key)).and_then(|v| v.as_mapping())
}

fn parse_tracker(map: Option<&Mapping>) -> Result<TrackerConfig, ConfigError> {
    let m = empty_if_none(map);
    let kind_raw = m
        .get(Value::from("kind"))
        .and_then(|v| v.as_str())
        .unwrap_or("linear");
    let kind = TrackerKind::parse(kind_raw);

    let endpoint = m
        .get(Value::from("endpoint"))
        .and_then(|v| v.as_str())
        .unwrap_or("https://api.linear.app/graphql")
        .to_string();

    let api_key_raw = m
        .get(Value::from("api_key"))
        .and_then(|v| v.as_str())
        .map(str::to_string);
    let api_key = api_key_raw.as_deref().map(resolve_env);
    let api_key = match api_key {
        Some(s) if s.is_empty() => None,
        other => other,
    };

    let project_slug = m
        .get(Value::from("project_slug"))
        .and_then(|v| v.as_str())
        .map(str::to_string);

    let active_states = string_list(m.get(Value::from("active_states")))
        .unwrap_or_else(|| vec!["Todo".into(), "In Progress".into()]);
    let terminal_states = string_list(m.get(Value::from("terminal_states"))).unwrap_or_else(|| {
        vec![
            "Closed".into(),
            "Cancelled".into(),
            "Canceled".into(),
            "Duplicate".into(),
            "Done".into(),
        ]
    });

    Ok(TrackerConfig {
        kind,
        endpoint,
        api_key,
        project_slug,
        active_states,
        terminal_states,
    })
}

fn parse_polling(map: Option<&Mapping>) -> Result<PollingConfig, ConfigError> {
    let m = empty_if_none(map);
    let interval_ms = m
        .get(Value::from("interval_ms"))
        .and_then(|v| v.as_u64())
        .unwrap_or(30_000);
    Ok(PollingConfig { interval_ms })
}

fn parse_workspace(
    map: Option<&Mapping>,
    workflow_dir: &Path,
) -> Result<WorkspaceConfig, ConfigError> {
    let m = empty_if_none(map);
    let raw = m
        .get(Value::from("root"))
        .and_then(|v| v.as_str())
        .map(str::to_string);

    let resolved = match raw {
        Some(value) => expand_path(&value, workflow_dir)?,
        None => default_workspace_root(),
    };
    Ok(WorkspaceConfig { root: resolved })
}

fn parse_hooks(map: Option<&Mapping>) -> Result<HooksConfig, ConfigError> {
    let m = empty_if_none(map);
    let timeout_ms = match m.get(Value::from("timeout_ms")) {
        Some(v) => v.as_u64().ok_or_else(|| ConfigError::InvalidValue {
            field: "hooks.timeout_ms".into(),
            reason: "expected positive integer milliseconds".into(),
        })?,
        None => 60_000,
    };
    Ok(HooksConfig {
        after_create: optional_string(&m, "after_create"),
        before_run: optional_string(&m, "before_run"),
        after_run: optional_string(&m, "after_run"),
        before_remove: optional_string(&m, "before_remove"),
        timeout_ms,
    })
}

fn parse_agent(map: Option<&Mapping>) -> Result<AgentConfig, ConfigError> {
    let m = empty_if_none(map);
    let max_concurrent_agents = m
        .get(Value::from("max_concurrent_agents"))
        .and_then(|v| v.as_u64())
        .unwrap_or(10) as usize;
    let max_turns = match m.get(Value::from("max_turns")) {
        Some(v) => {
            let n = v.as_u64().unwrap_or(0);
            if n == 0 {
                return Err(ConfigError::InvalidValue {
                    field: "agent.max_turns".into(),
                    reason: "must be a positive integer".into(),
                });
            }
            n as u32
        }
        None => 20,
    };
    let max_retry_backoff_ms = m
        .get(Value::from("max_retry_backoff_ms"))
        .and_then(|v| v.as_u64())
        .unwrap_or(300_000);

    let mut by_state = BTreeMap::new();
    if let Some(Value::Mapping(inner)) = m.get(Value::from("max_concurrent_agents_by_state")) {
        for (k, v) in inner {
            let key = match k {
                Value::String(s) => s.to_lowercase(),
                _ => continue,
            };
            let n = match v.as_u64() {
                Some(n) if n > 0 => n as usize,
                _ => continue,
            };
            by_state.insert(key, n);
        }
    }

    Ok(AgentConfig {
        max_concurrent_agents,
        max_turns,
        max_retry_backoff_ms,
        max_concurrent_agents_by_state: by_state,
    })
}

fn parse_codex(map: Option<&Mapping>) -> Result<CodexConfig, ConfigError> {
    let m = empty_if_none(map);
    let command = m
        .get(Value::from("command"))
        .and_then(|v| v.as_str())
        .unwrap_or("codex app-server")
        .to_string();
    Ok(CodexConfig {
        command,
        approval_policy: m.get(Value::from("approval_policy")).cloned(),
        thread_sandbox: m.get(Value::from("thread_sandbox")).cloned(),
        turn_sandbox_policy: m.get(Value::from("turn_sandbox_policy")).cloned(),
        turn_timeout_ms: m
            .get(Value::from("turn_timeout_ms"))
            .and_then(|v| v.as_u64())
            .unwrap_or(3_600_000),
        read_timeout_ms: m
            .get(Value::from("read_timeout_ms"))
            .and_then(|v| v.as_u64())
            .unwrap_or(5_000),
        stall_timeout_ms: m
            .get(Value::from("stall_timeout_ms"))
            .and_then(|v| v.as_i64())
            .unwrap_or(300_000),
    })
}

fn parse_server(map: Option<&Mapping>) -> Result<ServerConfig, ConfigError> {
    let port = match map.and_then(|m| m.get(Value::from("port"))) {
        Some(v) => match v.as_u64() {
            Some(n) if n <= u16::MAX as u64 => Some(n as u16),
            _ => {
                return Err(ConfigError::InvalidValue {
                    field: "server.port".into(),
                    reason: "expected integer in 0..=65535".into(),
                });
            }
        },
        None => None,
    };
    Ok(ServerConfig { port })
}

fn optional_string(m: &Mapping, key: &str) -> Option<String> {
    m.get(Value::from(key))
        .and_then(|v| v.as_str())
        .map(str::to_string)
}

fn string_list(v: Option<&Value>) -> Option<Vec<String>> {
    let seq = v?.as_sequence()?;
    Some(
        seq.iter()
            .filter_map(|v| v.as_str().map(str::to_string))
            .collect(),
    )
}

fn empty_if_none(map: Option<&Mapping>) -> std::borrow::Cow<'_, Mapping> {
    match map {
        Some(m) => std::borrow::Cow::Borrowed(m),
        None => std::borrow::Cow::Owned(Mapping::new()),
    }
}

fn default_workspace_root() -> PathBuf {
    std::env::temp_dir().join("symphony_workspaces")
}

/// SPEC §6.1: only resolve `$VAR` indirection where a value explicitly uses it.
fn resolve_env(raw: &str) -> String {
    if let Some(name) = raw.strip_prefix('$') {
        std::env::var(name).unwrap_or_default()
    } else {
        raw.to_string()
    }
}

/// SPEC §6.1 path coercion: `~`, `$VAR`, then resolve relative paths against
/// the directory containing `WORKFLOW.md`.
fn expand_path(raw: &str, workflow_dir: &Path) -> Result<PathBuf, ConfigError> {
    let resolved = if let Some(name) = raw.strip_prefix('$') {
        std::env::var(name).map_err(|_| ConfigError::InvalidValue {
            field: "workspace.root".into(),
            reason: format!("env var ${name} is unset"),
        })?
    } else {
        raw.to_string()
    };
    let expanded = shellexpand::tilde(&resolved).into_owned();
    let p = PathBuf::from(expanded);
    let abs = if p.is_absolute() {
        p
    } else {
        workflow_dir.join(p)
    };
    Ok(normalize_absolute(&abs))
}

fn normalize_absolute(p: &Path) -> PathBuf {
    use std::path::Component;
    let mut out = PathBuf::new();
    for comp in p.components() {
        match comp {
            Component::ParentDir => {
                out.pop();
            }
            Component::CurDir => {}
            other => out.push(other.as_os_str()),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::workflow::WorkflowLoader;
    use std::io::Write;
    use tempfile::NamedTempFile;

    fn config_from(yaml: &str) -> ServiceConfig {
        let body = format!("---\n{yaml}\n---\nbody\n");
        let mut f = NamedTempFile::new().unwrap();
        f.write_all(body.as_bytes()).unwrap();
        let def = WorkflowLoader::load(f.path()).unwrap();
        ServiceConfig::from_workflow(&def).unwrap()
    }

    #[test]
    fn applies_defaults_when_empty() {
        let cfg = config_from("{}");
        assert_eq!(cfg.polling.interval_ms, 30_000);
        assert_eq!(cfg.tracker.endpoint, "https://api.linear.app/graphql");
        assert_eq!(cfg.tracker.active_states, vec!["Todo", "In Progress"]);
        assert_eq!(cfg.agent.max_concurrent_agents, 10);
        assert_eq!(cfg.agent.max_turns, 20);
        assert_eq!(cfg.codex.command, "codex app-server");
        assert_eq!(cfg.hooks.timeout_ms, 60_000);
    }

    #[test]
    fn resolves_env_var_for_api_key() {
        std::env::set_var("SYMPHONY_TEST_KEY", "secret-token");
        let cfg = config_from(
            "tracker:\n  kind: linear\n  api_key: $SYMPHONY_TEST_KEY\n  project_slug: foo",
        );
        assert_eq!(cfg.tracker.api_key.as_deref(), Some("secret-token"));
        cfg.validate_for_dispatch().unwrap();
        std::env::remove_var("SYMPHONY_TEST_KEY");
    }

    #[test]
    fn missing_api_key_fails_validation() {
        let cfg = config_from("tracker:\n  kind: linear\n  project_slug: foo");
        let err = cfg.validate_for_dispatch().unwrap_err();
        assert!(matches!(err, ConfigError::MissingTrackerApiKey));
    }

    #[test]
    fn missing_project_slug_fails_validation() {
        std::env::set_var("SYMPHONY_TEST_KEY2", "x");
        let cfg = config_from("tracker:\n  kind: linear\n  api_key: $SYMPHONY_TEST_KEY2");
        let err = cfg.validate_for_dispatch().unwrap_err();
        assert!(matches!(err, ConfigError::MissingTrackerProjectSlug));
        std::env::remove_var("SYMPHONY_TEST_KEY2");
    }

    #[test]
    fn unsupported_tracker_kind_fails_validation() {
        let cfg = config_from("tracker:\n  kind: jira");
        let err = cfg.validate_for_dispatch().unwrap_err();
        assert!(matches!(err, ConfigError::UnsupportedTrackerKind(_)));
    }

    #[test]
    fn empty_codex_command_fails_validation() {
        std::env::set_var("SYMPHONY_TEST_KEY3", "k");
        let cfg = config_from(
            "tracker:\n  kind: linear\n  api_key: $SYMPHONY_TEST_KEY3\n  project_slug: x\ncodex:\n  command: '   '",
        );
        let err = cfg.validate_for_dispatch().unwrap_err();
        assert!(matches!(err, ConfigError::EmptyCodexCommand));
        std::env::remove_var("SYMPHONY_TEST_KEY3");
    }

    #[test]
    fn per_state_concurrency_normalizes_keys_and_skips_invalid() {
        let cfg = config_from(
            "agent:\n  max_concurrent_agents_by_state:\n    \"In Progress\": 3\n    Todo: 2\n    bogus: 0\n    invalid: hello\n",
        );
        let map = &cfg.agent.max_concurrent_agents_by_state;
        assert_eq!(map.get("in progress").copied(), Some(3));
        assert_eq!(map.get("todo").copied(), Some(2));
        assert!(!map.contains_key("bogus"));
        assert!(!map.contains_key("invalid"));
    }

    #[test]
    fn rejects_zero_max_turns() {
        let yaml = "agent:\n  max_turns: 0";
        let body = format!("---\n{yaml}\n---\nbody");
        let mut f = NamedTempFile::new().unwrap();
        f.write_all(body.as_bytes()).unwrap();
        let def = WorkflowLoader::load(f.path()).unwrap();
        let err = ServiceConfig::from_workflow(&def).unwrap_err();
        assert!(matches!(err, ConfigError::InvalidValue { .. }));
    }
}
