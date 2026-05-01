//! Smoke tests for the `symphony` binary covering SPEC §17.7 lifecycle
//! checks.

use std::io::Write;
use std::process::Command;

fn binary() -> std::path::PathBuf {
    std::path::PathBuf::from(env!("CARGO_BIN_EXE_symphony"))
}

#[test]
fn nonexistent_workflow_exits_with_code_2() {
    let out = Command::new(binary())
        .arg("/no/such/workflow.md")
        .output()
        .unwrap();
    assert!(!out.status.success());
    assert_eq!(out.status.code(), Some(2));
    let stderr = String::from_utf8_lossy(&out.stderr);
    assert!(stderr.contains("workflow file not found"));
}

#[test]
fn missing_default_workflow_in_cwd_exits_with_code_2() {
    let tmp = tempfile::tempdir().unwrap();
    let out = Command::new(binary())
        .current_dir(tmp.path())
        .output()
        .unwrap();
    assert!(!out.status.success());
    assert_eq!(out.status.code(), Some(2));
}

#[test]
fn workflow_with_unsupported_tracker_kind_fails_validation() {
    let tmp = tempfile::tempdir().unwrap();
    let workflow = tmp.path().join("WORKFLOW.md");
    let mut f = std::fs::File::create(&workflow).unwrap();
    writeln!(f, "---").unwrap();
    writeln!(f, "tracker:").unwrap();
    writeln!(f, "  kind: jira").unwrap();
    writeln!(f, "---").unwrap();
    writeln!(f, "body").unwrap();

    let out = Command::new(binary()).arg(&workflow).output().unwrap();
    assert!(!out.status.success());
    let stderr = String::from_utf8_lossy(&out.stderr);
    assert!(stderr.contains("unsupported tracker"));
}

#[test]
fn doctor_with_missing_workflow_exits_nonzero_and_prints_check() {
    let out = Command::new(binary())
        .arg("doctor")
        .arg("/no/such/workflow.md")
        .output()
        .unwrap();
    assert!(!out.status.success());
    let stdout = String::from_utf8_lossy(&out.stdout);
    assert!(stdout.contains("workflow file loadable"));
    assert!(stdout.contains("✗"));
    assert!(stdout.contains("check(s) failed"));
}

#[test]
fn doctor_reports_missing_dispatch_fields() {
    let tmp = tempfile::tempdir().unwrap();
    let workflow = tmp.path().join("WORKFLOW.md");
    let mut f = std::fs::File::create(&workflow).unwrap();
    writeln!(f, "---").unwrap();
    writeln!(f, "tracker:").unwrap();
    writeln!(f, "  kind: linear").unwrap();
    writeln!(f, "  project_slug: demo").unwrap();
    writeln!(f, "---").unwrap();
    writeln!(f, "body").unwrap();

    let out = Command::new(binary())
        .arg("doctor")
        .arg(&workflow)
        .output()
        .unwrap();
    assert!(!out.status.success());
    let stdout = String::from_utf8_lossy(&out.stdout);
    assert!(stdout.contains("dispatch preflight"));
    assert!(stdout.contains("api_key"));
}

#[test]
fn workflow_with_missing_api_key_fails_preflight() {
    let tmp = tempfile::tempdir().unwrap();
    let workflow = tmp.path().join("WORKFLOW.md");
    let mut f = std::fs::File::create(&workflow).unwrap();
    writeln!(f, "---").unwrap();
    writeln!(f, "tracker:").unwrap();
    writeln!(f, "  kind: linear").unwrap();
    writeln!(f, "  project_slug: demo").unwrap();
    writeln!(f, "---").unwrap();
    writeln!(f, "body").unwrap();

    let out = Command::new(binary()).arg(&workflow).output().unwrap();
    assert!(!out.status.success());
    let stderr = String::from_utf8_lossy(&out.stderr);
    assert!(stderr.contains("preflight failed"));
}
