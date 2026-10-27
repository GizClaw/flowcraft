//! Reload on TLS material changes.
//!
//! A rotation is normally two writes (certificate, then key) that must not be
//! observed half-done, so events are debounced before they trigger a reload.
//! Only the bootstrap secret directory is watched: it is the host's material,
//! and `SIGHUP` remains the manual equivalent.
//!
//! Two bounds keep the watch from becoming a reload loop. Writer *artifacts*
//! (`<random>.tmp` from an atomic write, the dotted files a keychain keeps beside
//! its sealed values) never trigger: they are about to disappear. And a rebuild
//! is expensive, so two reloads can never be closer than
//! [`MIN_RELOAD_INTERVAL`], however the directory churns.

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

/// The shortest gap between two reloads, however much the directory churns.
///
/// `DEBOUNCE` only waits for quiet: a directory that changes continuously (a
/// keychain store rewriting its index on every rotation, a craft touching a file
/// per second) would otherwise rebuild the whole deployment every half second,
/// and a rebuild is seconds of work.
const MIN_RELOAD_INTERVAL: Duration = Duration::from_secs(2);

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
        let mut last_reload: Option<tokio::time::Instant> = None;
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
            if let Some(last) = last_reload {
                let earliest = last + MIN_RELOAD_INTERVAL;
                let now = tokio::time::Instant::now();
                if now < earliest {
                    tokio::time::sleep_until(earliest).await;
                }
            }
            last_reload = Some(tokio::time::Instant::now());
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
    let lifecycle = matches!(
        event.kind,
        EventKind::Create(_) | EventKind::Modify(_) | EventKind::Remove(_) | EventKind::Any
    );
    // An atomic write lands as a temp name plus a rename, and a keychain store
    // keeps `.key` and `.sealed-*.tmp` beside its values. Those are writer
    // artifacts: reloading for one would rebuild the deployment for a file that
    // is about to be gone. A plain data file is still a real change, which is
    // why this filters by name instead of trying to guess the configured secret
    // names (the watcher never sees them).
    lifecycle && event.paths.iter().any(|path| !is_artifact(path))
}

/// Whether this path is a writer's scratch file rather than material.
fn is_artifact(path: &Path) -> bool {
    path.file_name()
        .and_then(|name| name.to_str())
        .is_some_and(|name| name.starts_with('.') || name.ends_with(".tmp"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use notify::event::{CreateKind, ModifyKind, RemoveKind};
    use std::path::PathBuf;

    fn event(kind: EventKind, paths: &[&str]) -> Event {
        let mut event = Event::new(kind);
        event.paths = paths.iter().map(PathBuf::from).collect();
        event
    }

    fn created(name: &str) -> Event {
        event(EventKind::Create(CreateKind::File), &[name])
    }

    #[test]
    fn content_and_lifecycle_events_trigger() {
        assert!(affects_secrets(&created("/secrets/tls.crt")));
        assert!(affects_secrets(&event(
            EventKind::Modify(ModifyKind::Data(notify::event::DataChange::Content)),
            &["/secrets/tls.key"],
        )));
        assert!(affects_secrets(&event(
            EventKind::Remove(RemoveKind::File),
            &["/secrets/tls.key"],
        )));
        assert!(!affects_secrets(&event(
            EventKind::Access(notify::event::AccessKind::Read),
            &["/secrets/tls.key"],
        )));
    }

    /// The regression: an atomic write is a temp file plus a rename, so the temp
    /// file must not be able to trigger a rebuild on its own.
    #[test]
    fn writer_artifacts_do_not_trigger() {
        assert!(!affects_secrets(&created("/secrets/.sealed-9f3a.tmp")));
        assert!(!affects_secrets(&created("/secrets/tls.key.tmp")));
        // The keychain's master key lives beside the sealed values.
        assert!(!affects_secrets(&created("/secrets/.key")));
        // An event with no path cannot be judged, so it does not trigger either.
        assert!(!affects_secrets(&Event::new(EventKind::Any)));
        // The rename itself is the real write, and its destination is material.
        assert!(affects_secrets(&event(
            EventKind::Modify(ModifyKind::Name(notify::event::RenameMode::Both)),
            &["/secrets/.sealed-9f3a.tmp", "/secrets/tls.crt"],
        )));
    }
}
