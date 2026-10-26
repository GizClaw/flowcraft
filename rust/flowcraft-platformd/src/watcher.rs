//! Reload on TLS material changes.
//!
//! A rotation is normally two writes (certificate, then key) that must not be
//! observed half-done, so events are debounced before they trigger a reload.
//! Only the bootstrap secret directory is watched: it is the host's material,
//! and `SIGHUP` remains the manual equivalent.

use std::path::Path;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use flowcraft::error::{Error, Result};
use flowcraft::logerr;
use notify::{Event, EventKind, RecursiveMode, Watcher};

use crate::supervisor::Supervisor;

/// How long to wait for the writes of one rotation to settle.
const DEBOUNCE: Duration = Duration::from_millis(500);

/// A running watcher. Dropping it stops watching.
pub struct ReloadWatcher {
    _watcher: notify::RecommendedWatcher,
    task: tokio::task::JoinHandle<()>,
}

impl std::fmt::Debug for ReloadWatcher {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("ReloadWatcher")
    }
}

impl ReloadWatcher {
    /// Stop watching.
    pub async fn stop(self) {
        self.task.abort();
        let _ = self.task.await;
    }
}

/// Watch `dir` and reload the deployment when a secret file changes.
pub fn watch(
    dir: &Path,
    supervisor: Arc<Supervisor>,
    ready: Arc<AtomicBool>,
) -> Result<ReloadWatcher> {
    let (sender, mut receiver) = tokio::sync::mpsc::unbounded_channel();
    let mut watcher = notify::recommended_watcher(move |event: notify::Result<Event>| {
        if let Ok(event) = event
            && affects_secrets(&event)
        {
            let _ = sender.send(());
        }
    })
    .map_err(|error| Error::not_available(format!("secret watcher: {error}")))?;
    watcher
        .watch(dir, RecursiveMode::Recursive)
        .map_err(|error| {
            Error::not_available(format!("secret watcher {}: {error}", dir.display()))
        })?;

    let task = tokio::spawn(async move {
        while receiver.recv().await.is_some() {
            // Coalesce the rest of this rotation: cert and key are two writes.
            loop {
                tokio::select! {
                    Some(()) = receiver.recv() => {}
                    _ = tokio::time::sleep(DEBOUNCE) => break,
                }
            }
            if !ready.load(Ordering::SeqCst) {
                tracing::debug!("secret change ignored while draining");
                continue;
            }
            match supervisor.reload().await {
                Ok(outcome) => tracing::info!(
                    flowcraft.gen.previous = outcome.previous,
                    flowcraft.gen = outcome.current,
                    flowcraft.resources = outcome.resources,
                    flowcraft.gen.drained = outcome.drained,
                    "reloaded after a secret change"
                ),
                Err(failure) => logerr!(
                    error,
                    failure,
                    "reload after a secret change failed; keeping the running generation"
                ),
            }
        }
    });

    Ok(ReloadWatcher {
        _watcher: watcher,
        task,
    })
}

/// Whether an event can change a secret: content, creation, removal, rename.
fn affects_secrets(event: &Event) -> bool {
    matches!(
        event.kind,
        EventKind::Create(_) | EventKind::Modify(_) | EventKind::Remove(_) | EventKind::Any
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use notify::event::{CreateKind, ModifyKind, RemoveKind};

    fn event(kind: EventKind) -> Event {
        Event::new(kind)
    }

    #[test]
    fn content_and_lifecycle_events_trigger() {
        assert!(affects_secrets(&event(EventKind::Create(CreateKind::File))));
        assert!(affects_secrets(&event(EventKind::Modify(
            ModifyKind::Data(notify::event::DataChange::Content)
        ))));
        assert!(affects_secrets(&event(EventKind::Remove(RemoveKind::File))));
        assert!(!affects_secrets(&event(EventKind::Access(
            notify::event::AccessKind::Read
        ))));
    }
}
