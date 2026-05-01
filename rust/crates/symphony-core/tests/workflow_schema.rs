//! Validate the WORKFLOW.md JSON Schema against fixture files.
//!
//! The schema lives at `docs/workflow.schema.json` (top-level) and is the
//! published interop contract that editors (VS Code, Zed) consume. This
//! test:
//!
//! 1. Parses each fixture's YAML front matter via the workflow loader.
//! 2. Converts the resulting `serde_yaml::Mapping` to a `serde_json::Value`.
//! 3. Validates against the published schema.
//!
//! Files named `*_invalid_*` or `invalid_*` are expected to FAIL validation;
//! everything else is expected to pass.

use std::path::{Path, PathBuf};

use jsonschema::{Draft, JSONSchema};
use serde_json::Value;
use symphony_core::workflow::WorkflowLoader;

const SCHEMA_RELATIVE: &str = "../../../docs/workflow.schema.json";
const FIXTURES_DIR: &str = "tests/fixtures";

fn schema_path() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join(SCHEMA_RELATIVE)
}

fn fixtures_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join(FIXTURES_DIR)
}

fn load_schema() -> JSONSchema {
    let raw = std::fs::read_to_string(schema_path()).expect("schema file");
    let value: Value = serde_json::from_str(&raw).expect("schema is JSON");
    JSONSchema::options()
        .with_draft(Draft::Draft202012)
        .compile(&value)
        .expect("schema compiles")
}

fn yaml_to_json(v: &serde_yaml::Value) -> Value {
    use serde_json::Value as J;
    use serde_yaml::Value as Y;
    match v {
        Y::Null => J::Null,
        Y::Bool(b) => J::Bool(*b),
        Y::Number(n) => {
            if let Some(i) = n.as_i64() {
                J::from(i)
            } else if let Some(u) = n.as_u64() {
                J::from(u)
            } else if let Some(f) = n.as_f64() {
                serde_json::Number::from_f64(f)
                    .map(J::Number)
                    .unwrap_or(J::Null)
            } else {
                J::Null
            }
        }
        Y::String(s) => J::String(s.clone()),
        Y::Sequence(items) => J::Array(items.iter().map(yaml_to_json).collect()),
        Y::Mapping(m) => {
            let mut out = serde_json::Map::new();
            for (k, val) in m {
                if let Some(key) = k.as_str() {
                    out.insert(key.to_string(), yaml_to_json(val));
                }
            }
            J::Object(out)
        }
        Y::Tagged(t) => yaml_to_json(&t.value),
    }
}

fn validate_fixture(schema: &JSONSchema, path: &Path) -> Result<(), String> {
    let def = WorkflowLoader::load(path).map_err(|e| format!("loader: {e}"))?;
    let yaml = serde_yaml::Value::Mapping(def.config);
    let json = yaml_to_json(&yaml);
    if let Err(errors) = schema.validate(&json) {
        let summary = errors
            .map(|e| format!("- {}: {}", e.instance_path, e))
            .collect::<Vec<_>>()
            .join("\n");
        return Err(summary);
    }
    Ok(())
}

#[test]
fn schema_compiles() {
    let _ = load_schema();
}

#[test]
fn valid_fixtures_pass_schema() {
    let schema = load_schema();
    let dir = fixtures_dir();
    let mut checked = 0;
    for entry in std::fs::read_dir(&dir).expect("fixtures dir") {
        let entry = entry.unwrap();
        let path = entry.path();
        if path.extension().and_then(|s| s.to_str()) != Some("md") {
            continue;
        }
        let name = path.file_name().unwrap().to_string_lossy().to_string();
        if name.starts_with("invalid_") {
            continue;
        }
        match validate_fixture(&schema, &path) {
            Ok(()) => checked += 1,
            Err(msg) => panic!("fixture `{name}` failed schema validation:\n{msg}"),
        }
    }
    assert!(
        checked >= 4,
        "expected at least 4 valid fixtures, found {checked}"
    );
}

#[test]
fn invalid_fixtures_fail_schema() {
    let schema = load_schema();
    let dir = fixtures_dir();
    let mut checked = 0;
    for entry in std::fs::read_dir(&dir).expect("fixtures dir") {
        let entry = entry.unwrap();
        let path = entry.path();
        if path.extension().and_then(|s| s.to_str()) != Some("md") {
            continue;
        }
        let name = path.file_name().unwrap().to_string_lossy().to_string();
        if !name.starts_with("invalid_") {
            continue;
        }
        match validate_fixture(&schema, &path) {
            Err(_) => checked += 1,
            Ok(()) => panic!("fixture `{name}` passed schema validation but should have failed"),
        }
    }
    assert!(
        checked >= 2,
        "expected at least 2 invalid fixtures, found {checked}"
    );
}
