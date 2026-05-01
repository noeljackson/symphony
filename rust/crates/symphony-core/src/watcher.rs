//! SPEC §6.2: dynamic `WORKFLOW.md` reload.
//!
//! Watches the workflow file for changes and emits debounced reload events on
//! a tokio channel. Invalid reloads do not crash the watcher; they are surfaced
//! as `Err` events so the caller can keep the last known good config alive
//! (SPEC §6.2: "Invalid reloads MUST NOT crash the service; keep operating
//! with the last known good effective configuration").

use std::path::{Path, PathBuf};
use std::time::Duration;

use notify_debouncer_mini::{new_debouncer, notify::RecursiveMode, DebouncedEventKind};
use tokio::sync::mpsc;

use crate::config::ServiceConfig;
use crate::errors::ConfigError;
use crate::workflow::WorkflowLoader;

#[derive(Debug)]
pub enum ReloadEvent {
    Loaded(Box<ServiceConfig>),
    Failed(ConfigError),
}

pub struct WorkflowWatcher {
    pub events: mpsc::Receiver<ReloadEvent>,
    // Keep debouncer alive for the lifetime of the watcher.
    _debouncer: notify_debouncer_mini::Debouncer<notify::RecommendedWatcher>,
}

impl WorkflowWatcher {
    pub fn start(path: &Path) -> std::io::Result<Self> {
        let (tx, rx) = mpsc::channel(8);
        let watch_path: PathBuf = path.to_path_buf();
        let trigger = tx.clone();
        let trigger_path = watch_path.clone();

        // Send an initial load so the consumer always has a fresh value.
        let _ = trigger.try_send(load(&trigger_path));

        let mut debouncer = new_debouncer(
            Duration::from_millis(250),
            move |res: notify_debouncer_mini::DebounceEventResult| match res {
                Ok(events) => {
                    if events
                        .iter()
                        .any(|e| matches!(e.kind, DebouncedEventKind::Any))
                    {
                        let _ = trigger.blocking_send(load(&trigger_path));
                    }
                }
                Err(_errs) => {
                    // Filesystem watch errors are tolerated — caller will pick up
                    // changes on the next defensive reload.
                }
            },
        )
        .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e.to_string()))?;

        // Watch the parent directory non-recursively so we still get events
        // even if the file is replaced atomically (rename-into-place).
        let parent = watch_path
            .parent()
            .map(Path::to_path_buf)
            .unwrap_or_else(|| PathBuf::from("."));
        debouncer
            .watcher()
            .watch(&parent, RecursiveMode::NonRecursive)
            .map_err(|e| std::io::Error::new(std::io::ErrorKind::Other, e.to_string()))?;

        Ok(WorkflowWatcher {
            events: rx,
            _debouncer: debouncer,
        })
    }
}

fn load(path: &Path) -> ReloadEvent {
    match WorkflowLoader::load(path) {
        Ok(def) => match ServiceConfig::from_workflow(&def) {
            Ok(cfg) => ReloadEvent::Loaded(Box::new(cfg)),
            Err(e) => ReloadEvent::Failed(e),
        },
        Err(e) => ReloadEvent::Failed(e.into()),
    }
}
