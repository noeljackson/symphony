//! SPEC §5: workflow file loader.
//!
//! Splits a Markdown file into optional YAML front matter and a trimmed prompt
//! body. Returns the front-matter root object as a `serde_yaml::Mapping`.

use std::path::{Path, PathBuf};

use serde_yaml::{Mapping, Value};

use crate::errors::WorkflowError;

/// Parsed `WORKFLOW.md` payload (SPEC §4.1.2).
#[derive(Debug, Clone)]
pub struct WorkflowDefinition {
    /// Front-matter root object. Empty mapping if no front matter was present.
    pub config: Mapping,
    /// Trimmed Markdown body. Empty string if no body was present.
    pub prompt_template: String,
    /// Absolute path the workflow was loaded from.
    pub path: PathBuf,
}

pub struct WorkflowLoader;

impl WorkflowLoader {
    /// Load `WORKFLOW.md` from `path` and split front matter / body.
    ///
    /// SPEC §5.2 parsing rules:
    /// * if file starts with `---`, parse lines until the next `---` as YAML
    ///   front matter and treat the rest as the body;
    /// * otherwise treat the whole file as the body and use an empty config map;
    /// * front matter MUST decode to a map/object, else error;
    /// * body is trimmed before use.
    pub fn load(path: &Path) -> Result<WorkflowDefinition, WorkflowError> {
        let abs = path
            .canonicalize()
            .map_err(|e| WorkflowError::MissingWorkflowFile(format!("{}: {e}", path.display())))?;
        let raw = std::fs::read_to_string(&abs)
            .map_err(|e| WorkflowError::MissingWorkflowFile(format!("{}: {e}", abs.display())))?;
        let (config, body) = split_front_matter(&raw)?;
        Ok(WorkflowDefinition {
            config,
            prompt_template: body.trim().to_string(),
            path: abs,
        })
    }
}

fn split_front_matter(raw: &str) -> Result<(Mapping, String), WorkflowError> {
    // The fence MUST be the very first line.
    let starts_with_fence = raw.starts_with("---\n") || raw.starts_with("---\r\n") || raw == "---";
    if !starts_with_fence {
        return Ok((Mapping::new(), raw.to_string()));
    }

    let after_first = match raw.split_once('\n') {
        Some((_, rest)) => rest,
        None => "",
    };

    // Find the closing `---` line.
    let mut yaml_text = String::new();
    let mut body_text = String::new();
    let mut closed = false;
    for (idx, line) in after_first.split_inclusive('\n').enumerate() {
        let trimmed = line.trim_end_matches(['\r', '\n']);
        if !closed && trimmed == "---" {
            closed = true;
            continue;
        }
        if closed {
            body_text.push_str(line);
        } else {
            yaml_text.push_str(line);
        }
        let _ = idx; // suppress unused warning when body_text empty
    }

    if !closed {
        return Err(WorkflowError::WorkflowParseError(
            "front matter not terminated by closing `---`".to_string(),
        ));
    }

    let value: Value = serde_yaml::from_str(&yaml_text)
        .map_err(|e| WorkflowError::WorkflowParseError(e.to_string()))?;
    let mapping = match value {
        Value::Null => Mapping::new(),
        Value::Mapping(m) => m,
        _ => return Err(WorkflowError::WorkflowFrontMatterNotAMap),
    };
    Ok((mapping, body_text))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io::Write;
    use tempfile::NamedTempFile;

    fn write_tmp(contents: &str) -> NamedTempFile {
        let mut f = NamedTempFile::new().unwrap();
        f.write_all(contents.as_bytes()).unwrap();
        f
    }

    #[test]
    fn loads_with_front_matter() {
        let f = write_tmp("---\nfoo: 1\n---\nbody text\n");
        let wf = WorkflowLoader::load(f.path()).unwrap();
        assert_eq!(wf.config.get("foo").and_then(|v| v.as_i64()), Some(1));
        assert_eq!(wf.prompt_template, "body text");
    }

    #[test]
    fn loads_without_front_matter() {
        let f = write_tmp("just a prompt\n");
        let wf = WorkflowLoader::load(f.path()).unwrap();
        assert!(wf.config.is_empty());
        assert_eq!(wf.prompt_template, "just a prompt");
    }

    #[test]
    fn rejects_non_map_front_matter() {
        let f = write_tmp("---\n- a\n- b\n---\nbody\n");
        let err = WorkflowLoader::load(f.path()).unwrap_err();
        assert!(matches!(err, WorkflowError::WorkflowFrontMatterNotAMap));
    }

    #[test]
    fn rejects_unterminated_front_matter() {
        let f = write_tmp("---\nfoo: 1\nbody text\n");
        let err = WorkflowLoader::load(f.path()).unwrap_err();
        assert!(matches!(err, WorkflowError::WorkflowParseError(_)));
    }

    #[test]
    fn missing_file_is_typed_error() {
        let err = WorkflowLoader::load(std::path::Path::new("/no/such/file.md")).unwrap_err();
        assert!(matches!(err, WorkflowError::MissingWorkflowFile(_)));
    }
}
