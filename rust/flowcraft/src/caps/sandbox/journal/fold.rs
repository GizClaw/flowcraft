//! Folding raw filesystem notifications into **net state changes**.
//!
//! What a consumer wants is "which artifacts did this turn produce", not a
//! syscall trace. So:
//!
//! - a file created and then written is **one** `create`;
//! - an attribute change (`chmod`, `touch` on an existing file) is never an
//!   event — a chip saying "written" for a `cat` would be a false positive;
//! - a rename is one `rename` carrying its origin when the backend can pair the
//!   two halves, and a `rename` with no origin when it cannot (the capability
//!   table says which);
//! - a removal is one `remove`.
//!
//! The fold is a pure state machine over [`RawEvent`], so every rule above is
//! asserted without touching the filesystem.

use std::collections::{HashSet, VecDeque};
use std::path::{Path, PathBuf};

use super::FileOp;

/// The shape of one raw notification, independent of the watching library.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) enum RawKind {
    /// The path appeared.
    Created { is_dir: bool },
    /// The content of an existing path changed.
    Modified,
    /// The path disappeared.
    Removed { is_dir: bool },
    /// The path moved; `from` is `None` when the backend cannot pair the halves.
    Renamed { from: Option<PathBuf>, is_dir: bool },
}

/// One raw notification.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct RawEvent {
    /// What happened.
    pub kind: RawKind,
    /// The path it happened to (the destination for a rename).
    pub path: PathBuf,
}

/// One folded change, ready for the ring to number.
#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Folded {
    pub op: FileOp,
    pub path: String,
    pub old_path: Option<String>,
    pub is_dir: bool,
}

/// How many created paths to remember.
///
/// The set exists to fold "created then written" into one event. Bounding it
/// only means a later write is reported as a `write` rather than being swallowed
/// — the safe direction.
const MAX_CREATED: usize = 65536;

/// The folding state machine.
#[derive(Debug)]
pub(crate) struct Fold {
    created: HashSet<PathBuf>,
    order: VecDeque<PathBuf>,
}

impl Default for Fold {
    fn default() -> Self {
        Self::new()
    }
}

impl Fold {
    pub(crate) fn new() -> Self {
        Self {
            created: HashSet::new(),
            order: VecDeque::new(),
        }
    }

    /// Fold one raw notification into at most one net change.
    pub(crate) fn apply(&mut self, event: RawEvent) -> Option<Folded> {
        match event.kind {
            RawKind::Created { is_dir } => {
                if !self.remember(&event.path) {
                    // The backend repeated itself (inotify can report the same
                    // creation more than once for a directory tree).
                    return None;
                }
                Some(Folded {
                    op: FileOp::Create,
                    path: event.path.display().to_string(),
                    old_path: None,
                    is_dir,
                })
            }
            RawKind::Modified => {
                if self.created.contains(&event.path) {
                    // Created and written: still one `create`.
                    return None;
                }
                Some(Folded {
                    op: FileOp::Write,
                    path: event.path.display().to_string(),
                    old_path: None,
                    is_dir: false,
                })
            }
            RawKind::Removed { is_dir } => {
                if is_dir {
                    // A removed directory reports nothing per child, so the marks
                    // of its subtree must go with it — otherwise re-creating a
                    // child under a re-created directory is silently swallowed.
                    self.forget_subtree(&event.path);
                } else {
                    self.forget(&event.path);
                }
                Some(Folded {
                    op: FileOp::Remove,
                    path: event.path.display().to_string(),
                    old_path: None,
                    is_dir,
                })
            }
            RawKind::Renamed { from, is_dir } => {
                let old_path = from.as_ref().map(|path| path.display().to_string());
                if let Some(from) = &from {
                    if is_dir {
                        self.forget_subtree(from);
                    } else {
                        self.forget(from);
                    }
                }
                self.remember(&event.path);
                Some(Folded {
                    op: FileOp::Rename,
                    path: event.path.display().to_string(),
                    old_path,
                    is_dir,
                })
            }
        }
    }

    /// Remember a created path; `false` when it was already known.
    fn remember(&mut self, path: &Path) -> bool {
        if !self.created.insert(path.to_path_buf()) {
            return false;
        }
        self.order.push_back(path.to_path_buf());
        while self.order.len() > MAX_CREATED {
            if let Some(oldest) = self.order.pop_front() {
                self.created.remove(&oldest);
            }
        }
        true
    }

    fn forget(&mut self, path: &Path) {
        self.created.remove(path);
    }

    /// Forget `path` and everything under it.
    fn forget_subtree(&mut self, path: &Path) {
        self.created.retain(|known| !known.starts_with(path));
        self.order.retain(|known| !known.starts_with(path));
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn created(path: &str) -> RawEvent {
        RawEvent {
            kind: RawKind::Created { is_dir: false },
            path: PathBuf::from(path),
        }
    }

    fn modified(path: &str) -> RawEvent {
        RawEvent {
            kind: RawKind::Modified,
            path: PathBuf::from(path),
        }
    }

    fn removed(path: &str) -> RawEvent {
        RawEvent {
            kind: RawKind::Removed { is_dir: false },
            path: PathBuf::from(path),
        }
    }

    #[test]
    fn create_then_write_is_one_create() {
        let mut fold = Fold::new();
        let first = fold.apply(created("/w/a")).expect("create");
        assert_eq!(first.op, FileOp::Create);
        assert_eq!(fold.apply(modified("/w/a")), None, "the write folds in");
        assert_eq!(fold.apply(modified("/w/a")), None, "and stays folded");
    }

    #[test]
    fn a_write_to_an_existing_file_is_a_write() {
        let mut fold = Fold::new();
        let folded = fold.apply(modified("/w/a")).expect("write");
        assert_eq!(folded.op, FileOp::Write);
        assert_eq!(folded.path, "/w/a");
    }

    #[test]
    fn a_repeated_creation_is_reported_once() {
        let mut fold = Fold::new();
        assert!(fold.apply(created("/w/dir")).is_some());
        assert_eq!(fold.apply(created("/w/dir")), None);
    }

    #[test]
    fn a_paired_rename_carries_its_origin() {
        let mut fold = Fold::new();
        let folded = fold
            .apply(RawEvent {
                kind: RawKind::Renamed {
                    from: Some(PathBuf::from("/w/a")),
                    is_dir: false,
                },
                path: PathBuf::from("/w/b"),
            })
            .expect("rename");
        assert_eq!(folded.op, FileOp::Rename);
        assert_eq!(folded.path, "/w/b");
        assert_eq!(folded.old_path.as_deref(), Some("/w/a"));
    }

    #[test]
    fn an_unpaired_rename_still_reports_a_rename() {
        let mut fold = Fold::new();
        let folded = fold
            .apply(RawEvent {
                kind: RawKind::Renamed {
                    from: None,
                    is_dir: false,
                },
                path: PathBuf::from("/w/b"),
            })
            .expect("rename");
        assert_eq!(folded.op, FileOp::Rename);
        assert_eq!(folded.old_path, None, "the origin is honestly unknown");
    }

    #[test]
    fn a_removal_ends_the_created_state() {
        let mut fold = Fold::new();
        fold.apply(created("/w/a"));
        assert!(fold.apply(removed("/w/a")).is_some());
        // A later write is no longer part of the creation.
        let folded = fold.apply(modified("/w/a")).expect("write");
        assert_eq!(folded.op, FileOp::Write);
    }

    #[test]
    fn a_rename_ends_the_created_state_of_the_origin() {
        let mut fold = Fold::new();
        fold.apply(created("/w/a"));
        fold.apply(RawEvent {
            kind: RawKind::Renamed {
                from: Some(PathBuf::from("/w/a")),
                is_dir: false,
            },
            path: PathBuf::from("/w/b"),
        });
        // Writing the *destination* is part of the rename, not a new write.
        assert_eq!(fold.apply(modified("/w/b")), None);
    }

    #[test]
    fn re_creating_a_child_of_a_removed_directory_is_reported() {
        let mut fold = Fold::new();
        fold.apply(RawEvent {
            kind: RawKind::Created { is_dir: true },
            path: PathBuf::from("d"),
        });
        fold.apply(created("d/f"));
        // `rm -rf d` sends one event for the directory, no per-child removals.
        fold.apply(RawEvent {
            kind: RawKind::Removed { is_dir: true },
            path: PathBuf::from("d"),
        });
        assert!(
            fold.apply(created("d/f")).is_some(),
            "a mark under a removed directory must not survive it"
        );
    }

    #[test]
    fn a_directory_is_marked_as_one() {
        let mut fold = Fold::new();
        let folded = fold
            .apply(RawEvent {
                kind: RawKind::Created { is_dir: true },
                path: PathBuf::from("/w/dir"),
            })
            .expect("create");
        assert!(folded.is_dir);
    }
}
