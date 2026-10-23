//! The `notify`-based source.
//!
//! `notify` is the syscall layer (inotify on Linux, FSEvents on macOS, kqueue on
//! the BSDs, ReadDirectoryChangesW on Windows); the semantics a consumer depends
//! on are not, which is why they live in [`super::super::fold`] and
//! [`super::super::ring`].
//!
//! What this file must get right, and only this:
//!
//! - an event is translated into a [`RawEvent`] honestly, or dropped when the
//!   platform gave us something we cannot classify (an `Access`, a metadata
//!   touch, an `Any` we cannot attribute);
//! - an excluded path prefix never produces an event (the cost control);
//! - a source error becomes a gap, never silence.

use std::path::{Path, PathBuf};

use flowcraft_error::{Error, Result};
use notify::event::{CreateKind, ModifyKind, RemoveKind, RenameMode};
use notify::{EventKind, RecursiveMode, Watcher};

use super::super::{Config, GapReason, RawEvent, RawKind};
use super::Sink;

/// A running watcher.
///
/// Dropping it stops watching: the handle owns the platform watcher.
pub(crate) struct NotifySource {
    // Kept alive for the lifetime of the journal; the field is never read.
    _watcher: notify::RecommendedWatcher,
}

impl NotifySource {
    /// Start watching `root` recursively.
    ///
    /// Fails when the platform has no watcher or the root cannot be watched —
    /// a journal that watches nothing must not exist.
    pub(crate) fn start(root: &Path, config: &Config, sink: Sink) -> Result<Self> {
        if !root.is_dir() {
            return Err(Error::not_found(format!(
                "sandbox: journal root {} is not a directory",
                root.display()
            )));
        }
        let excluded = excluded_prefixes(root, &config.exclude);
        let mut watcher =
            notify::recommended_watcher(move |result: notify::Result<notify::Event>| {
                let incoming = match result {
                    Ok(event) => {
                        // A dropped-event notification arrives as `Ok`, without
                        // paths: inotify's `Q_OVERFLOW` and FSEvents' "must scan
                        // subdirs" both look like this, and `to_raw` would drop
                        // them for having no path. That is the one case the whole
                        // ring/gap machinery exists for.
                        if event.need_rescan() {
                            let _ = sink.send(super::Incoming::Gap(GapReason::Overflow));
                            return;
                        }
                        let Some(raw) = to_raw(&event) else {
                            return;
                        };
                        if excluded.iter().any(|prefix| raw.path.starts_with(prefix)) {
                            // Excluded: never an event, so never a gap.
                            return;
                        }
                        super::Incoming::Event(raw)
                    }
                    // The kernel queue overflowed, a watch could not be
                    // registered, or the backend failed: the consumer has to know
                    // coverage broke.
                    Err(error) => {
                        tracing::debug!(%error, "sandbox: journal source reported an error");
                        // The kernel's watch limit is a capacity problem, not a
                        // backend failure: the reader can act on the difference.
                        match error.kind {
                            notify::ErrorKind::MaxFilesWatch => {
                                super::Incoming::Gap(GapReason::Capacity)
                            }
                            _ => super::Incoming::Gap(GapReason::Backend),
                        }
                    }
                };
                // A closed receiver means the journal is shutting down.
                let _ = sink.send(incoming);
            })
            .map_err(|error| {
                Error::not_available(format!(
                    "sandbox: this platform has no file watcher: {error}"
                ))
            })?;

        watcher
            .watch(root, RecursiveMode::Recursive)
            .map_err(|error| {
                Error::not_available(format!("sandbox: cannot watch {}: {error}", root.display()))
            })?;
        Ok(Self { _watcher: watcher })
    }
}

/// Resolve the excluded prefixes against the root.
fn excluded_prefixes(root: &Path, exclude: &[PathBuf]) -> Vec<PathBuf> {
    exclude
        .iter()
        .map(|path| {
            if path.is_absolute() {
                path.clone()
            } else {
                root.join(path)
            }
        })
        .collect()
}

/// Translate one notification, or drop it.
fn to_raw(event: &notify::Event) -> Option<RawEvent> {
    let mut paths = event.paths.iter();
    let first = paths.next()?.clone();
    let second = paths.next().cloned();
    match &event.kind {
        EventKind::Create(kind) => Some(RawEvent {
            kind: RawKind::Created {
                is_dir: matches!(kind, CreateKind::Folder),
            },
            path: first,
        }),
        // Only content changes are net state changes; a metadata touch or a read
        // is not "the file was produced".
        EventKind::Modify(ModifyKind::Data(_)) => Some(RawEvent {
            kind: RawKind::Modified,
            path: first,
        }),
        EventKind::Modify(ModifyKind::Name(mode)) => match (mode, second) {
            // Both halves in one notification: the backend paired them.
            (RenameMode::Both, Some(to)) => Some(RawEvent {
                kind: RawKind::Renamed {
                    from: Some(first),
                    is_dir: false,
                },
                path: to,
            }),
            // Only the source is known: that path is gone, which is a true net
            // state statement. The matching destination arrives separately.
            (RenameMode::From, _) => Some(RawEvent {
                kind: RawKind::Removed { is_dir: false },
                path: first,
            }),
            // Destination only (FSEvents, kqueue): a rename with an unknown
            // origin, which the capability table declares.
            _ => Some(RawEvent {
                kind: RawKind::Renamed {
                    from: None,
                    is_dir: false,
                },
                path: first,
            }),
        },
        EventKind::Remove(kind) => Some(RawEvent {
            kind: RawKind::Removed {
                is_dir: matches!(kind, RemoveKind::Folder),
            },
            path: first,
        }),
        // Access, metadata changes, and unclassifiable events carry no net state
        // change, so they are not events at all.
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use notify::event::{AccessKind, DataChange, MetadataKind};

    fn event(kind: EventKind, paths: Vec<PathBuf>) -> notify::Event {
        let mut event = notify::Event::new(kind);
        for path in paths {
            event = event.add_path(path);
        }
        event
    }

    #[test]
    fn a_creation_is_classified() {
        let raw = to_raw(&event(
            EventKind::Create(CreateKind::File),
            vec![PathBuf::from("/w/f")],
        ))
        .expect("create");
        assert_eq!(raw.kind, RawKind::Created { is_dir: false });
        assert_eq!(raw.path, PathBuf::from("/w/f"));

        let raw = to_raw(&event(
            EventKind::Create(CreateKind::Folder),
            vec![PathBuf::from("/w/d")],
        ))
        .expect("create");
        assert!(matches!(raw.kind, RawKind::Created { is_dir: true }));
    }

    #[test]
    fn a_data_change_is_a_modification() {
        let raw = to_raw(&event(
            EventKind::Modify(ModifyKind::Data(DataChange::Content)),
            vec![PathBuf::from("/w/f")],
        ))
        .expect("modify");
        assert_eq!(raw.kind, RawKind::Modified);
    }

    #[test]
    fn a_metadata_touch_and_a_read_are_not_events() {
        assert!(
            to_raw(&event(
                EventKind::Modify(ModifyKind::Metadata(MetadataKind::Any)),
                vec![PathBuf::from("/w/f")],
            ))
            .is_none()
        );
        assert!(
            to_raw(&event(
                EventKind::Access(AccessKind::Read),
                vec![PathBuf::from("/w/f")],
            ))
            .is_none()
        );
    }

    #[test]
    fn a_paired_rename_carries_both_paths() {
        let raw = to_raw(&event(
            EventKind::Modify(ModifyKind::Name(RenameMode::Both)),
            vec![PathBuf::from("/w/a"), PathBuf::from("/w/b")],
        ))
        .expect("rename");
        assert_eq!(
            raw.kind,
            RawKind::Renamed {
                from: Some(PathBuf::from("/w/a")),
                is_dir: false
            }
        );
        assert_eq!(raw.path, PathBuf::from("/w/b"));
    }

    #[test]
    fn an_unpaired_rename_has_no_origin() {
        let raw = to_raw(&event(
            EventKind::Modify(ModifyKind::Name(RenameMode::Any)),
            vec![PathBuf::from("/w/b")],
        ))
        .expect("rename");
        assert_eq!(
            raw.kind,
            RawKind::Renamed {
                from: None,
                is_dir: false
            }
        );
    }

    #[test]
    fn only_the_source_of_a_rename_is_a_removal() {
        let raw = to_raw(&event(
            EventKind::Modify(ModifyKind::Name(RenameMode::From)),
            vec![PathBuf::from("/w/a")],
        ))
        .expect("removed");
        assert_eq!(raw.kind, RawKind::Removed { is_dir: false });
    }

    #[test]
    fn a_removal_is_classified() {
        let raw = to_raw(&event(
            EventKind::Remove(RemoveKind::Any),
            vec![PathBuf::from("/w/f")],
        ))
        .expect("remove");
        assert_eq!(raw.kind, RawKind::Removed { is_dir: false });
    }

    #[test]
    fn an_event_without_a_path_is_dropped() {
        assert!(to_raw(&notify::Event::new(EventKind::Create(CreateKind::Any))).is_none());
    }

    #[test]
    fn a_dropped_event_notification_is_recognisable() {
        // inotify's Q_OVERFLOW and FSEvents' "must scan subdirs" both arrive as an
        // `Ok` event with no paths and the rescan flag.
        let overflow = notify::Event::new(EventKind::Other).set_flag(notify::event::Flag::Rescan);
        assert!(overflow.need_rescan());
        assert!(to_raw(&overflow).is_none(), "it carries no path");
    }

    #[test]
    fn exclusions_are_resolved_against_the_root() {
        let root = Path::new("/srv/box");
        let prefixes = excluded_prefixes(
            root,
            &[PathBuf::from("target"), PathBuf::from("/tmp/cache")],
        );
        assert_eq!(
            prefixes,
            vec![
                PathBuf::from("/srv/box/target"),
                PathBuf::from("/tmp/cache")
            ]
        );
        assert!(Path::new("/srv/box/target/debug/x").starts_with(&prefixes[0]));
        assert!(!Path::new("/srv/box/targets/x").starts_with(&prefixes[0]));
    }
}
