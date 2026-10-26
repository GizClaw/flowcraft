//! The file journal: what a session actually produced.
//!
//! A runner with a journal reports the writes that happen inside its boundary as
//! a cursor-based, replayable stream of **net state changes** — one `create` for
//! "created and written", one `rename` for a move, nothing for a `chmod` or a
//! read. It is an observation stream, not a boundary: enforcement stays with the
//! backend, and the journal only reports what happened.
//!
//! Three guarantees hold for every reader:
//!
//! - `(after_seq, next_seq]` is always accounted for — every sequence number was
//!   either returned or declared lost in a [`Gap`];
//! - [`Journal::read`] never blocks, so a consumer drains on its own schedule;
//! - a filtered operation (by `ops` or by `exclude`) is **not** a gap: it was
//!   never an event, so nothing is missing.
//!
//! The syscall layer is `notify`; the semantics — folding, the retention ring,
//! the gap accounting — live here, because they are the part a consumer depends
//! on.

mod fold;
mod ring;
mod source;

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard};

use crate::error::{Error, Result};
use async_trait::async_trait;
use serde::Deserialize;
use serde_json::Value;

use crate::caps::sandbox::contract::JournalCapabilities;

pub(crate) use fold::{Fold, RawEvent, RawKind};
use ring::Ring;
pub(crate) use source::Incoming;

/// The default number of events kept readable for replay.
const DEFAULT_RETENTION: usize = 4096;
/// The default watch budget reported to callers.
const DEFAULT_WATCH_BUDGET: usize = 4096;

/// One class of observed change.
///
/// Only write-class operations become events: attribute changes (`chmod`,
/// `utimes`) and reads never do — reporting them would turn "the file was
/// inspected" into "the file was produced".
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum FileOp {
    /// The path appeared (created and written in one go is reported as this).
    Create,
    /// The content of an already-existing path changed.
    Write,
    /// The path moved; [`WriteEvent::old_path`] names where it came from when the
    /// backend can pair the halves.
    Rename,
    /// The path disappeared.
    Remove,
}

impl FileOp {
    /// The stable spelling used on the wire and in settings.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Create => "create",
            Self::Write => "write",
            Self::Rename => "rename",
            Self::Remove => "remove",
        }
    }

    /// Decode a wire or settings spelling.
    pub fn parse(raw: &str) -> Result<Self> {
        match raw {
            "create" => Ok(Self::Create),
            "write" => Ok(Self::Write),
            "rename" => Ok(Self::Rename),
            "remove" => Ok(Self::Remove),
            other => Err(Error::validation(format!(
                "sandbox: unknown journal op {other:?}"
            ))),
        }
    }
}

/// One observed change inside the runner's write boundary.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct WriteEvent {
    /// Monotonic sequence number; a cursor starts at `0`.
    pub seq: u64,
    /// When it was observed, nanoseconds since the epoch.
    pub at_unix_nano: u64,
    /// What happened.
    pub op: FileOp,
    /// Path under the root, `/`-separated and without a leading slash; a change
    /// under an explicit writable path outside the root carries that absolute
    /// path instead.
    pub path: String,
    /// The origin of a rename, when the backend can pair it.
    pub old_path: Option<String>,
    /// Whether the path is a directory.
    pub is_dir: bool,
    /// Filesystem identity, when the platform has one.
    pub dev: Option<u64>,
    /// Filesystem identity, when the platform has one.
    pub ino: Option<u64>,
    /// Size in bytes, when it still exists.
    pub size: Option<i64>,
    /// The session that made the change, when the backend can attribute it.
    ///
    /// Always empty today: inotify cannot name the writer without privileges and
    /// kqueue cannot name it at all, and a guess from timing would be a
    /// data-correctness bug.
    pub session: String,
}

/// Why coverage is incomplete.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum GapReason {
    /// The kernel's event queue overflowed.
    Overflow,
    /// The reader fell behind the retained window.
    Retention,
    /// A watch could not be registered (limits, descriptor budget, missing dir).
    Capacity,
    /// A watched subtree was moved or detached.
    WatchLost,
    /// The source itself failed.
    Backend,
}

impl GapReason {
    /// The stable spelling used on the wire.
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Overflow => "overflow",
            Self::Retention => "retention",
            Self::Capacity => "capacity",
            Self::WatchLost => "watch_lost",
            Self::Backend => "backend",
        }
    }

    /// Decode a wire spelling.
    pub fn parse(raw: &str) -> Result<Self> {
        match raw {
            "overflow" => Ok(Self::Overflow),
            "retention" => Ok(Self::Retention),
            "capacity" => Ok(Self::Capacity),
            "watch_lost" => Ok(Self::WatchLost),
            "backend" => Ok(Self::Backend),
            other => Err(Error::validation(format!(
                "sandbox: unknown journal gap reason {other:?}"
            ))),
        }
    }
}

/// A statement about coverage, not an error and not an empty range.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Gap {
    /// The first sequence number coverage is not guaranteed from.
    ///
    /// A retention loss has an unknown extent, so this can be **smaller** than
    /// the batch's `next_seq`: read it as "coverage after `first_missing - 1` is
    /// not guaranteed", and resume at `next_seq`.
    pub first_missing: u64,
    /// Why.
    pub reason: GapReason,
}

/// One read.
///
/// The cursor convention: `after_seq` is the **last sequence number the caller
/// has consumed** (`0` means "nothing yet"), and `next_seq` is what to pass next
/// time. A batch either delivers events or declares a [`Gap`], never both.
#[derive(Debug, Clone, Default, PartialEq, Eq)]
pub struct Batch {
    /// Events after the requested cursor, in order.
    pub events: Vec<WriteEvent>,
    /// The cursor to pass next time.
    ///
    /// With a retention gap it is the last cursor that is still covered, so the
    /// next read delivers the first retained event; with a source gap it is the
    /// hole's own sequence number, so passing it back accepts the loss and
    /// continues past it.
    pub next_seq: u64,
    /// Set when this batch declares a hole instead of delivering events.
    pub gap: Option<Gap>,
    /// Whether the journal is frozen: no further events will arrive, and the
    /// retained residue stays readable until the reader closes.
    pub closed: bool,
}

impl Batch {
    /// A batch that declares a hole and nothing else.
    #[must_use]
    pub fn is_gap(&self) -> bool {
        self.gap.is_some()
    }
}

/// A readable journal.
#[async_trait]
pub trait Journal: Send + Sync + std::fmt::Debug {
    /// Read from `after_seq`, at most `max` events. Never blocks.
    async fn read(&self, after_seq: u64, max: usize) -> Result<Batch>;

    /// Release this reader. The journal itself keeps running for its runner.
    async fn close(&self) -> Result<()>;
}

/// The resolved settings of one journal.
#[derive(Debug, Clone, Default, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Config {
    /// Directories whose subtree is never watched, relative to the root.
    ///
    /// A path prefix, not a glob: `target` excludes `target` and everything
    /// under it. This is the cost control, so it has no implicit default — an
    /// implicit filter would hide writes a deployment believes it is watching.
    #[serde(default)]
    pub exclude: Vec<PathBuf>,
    /// How many events stay readable for replay (default 4096).
    #[serde(default)]
    pub retention: Option<usize>,
    /// The watch budget, in the platform's own unit.
    ///
    /// Reported through [`JournalCapabilities::watch_budget`] and enforced by the
    /// platform: the watcher cannot enumerate its own registrations, so this is a
    /// declared bound rather than a mechanism.
    #[serde(default)]
    pub max_watch_set: Option<usize>,
    /// Only these operations become events. `None` keeps every one; a filtered op
    /// is never a gap.
    #[serde(default)]
    pub ops: Option<Vec<FileOp>>,
}

impl Config {
    /// Decode a settings subtree.
    pub fn parse(value: &Value) -> Result<Self> {
        let config: Self = crate::resource::settings(value)?;
        for (index, path) in config.exclude.iter().enumerate() {
            if path.as_os_str().is_empty() {
                return Err(Error::validation(format!(
                    "sandbox: journal.exclude[{index}] is empty"
                )));
            }
        }
        if config.retention == Some(0) {
            return Err(Error::validation(
                "sandbox: journal.retention must be positive",
            ));
        }
        if config.max_watch_set == Some(0) {
            return Err(Error::validation(
                "sandbox: journal.max_watch_set must be positive",
            ));
        }
        Ok(config)
    }

    pub(crate) fn resolved_retention(&self) -> usize {
        self.retention.unwrap_or(DEFAULT_RETENTION).max(1)
    }

    pub(crate) fn resolved_budget(&self) -> usize {
        self.max_watch_set.unwrap_or(DEFAULT_WATCH_BUDGET).max(1)
    }
}

/// Whether an operation passes an `ops` filter.
///
/// `None` and an empty list both mean "no filter": an empty `ops` is almost
/// certainly a configuration slip, and silently reporting nothing would be the
/// worst reading of it.
pub(crate) fn allows(ops: &Option<Vec<FileOp>>, op: FileOp) -> bool {
    ops.as_ref()
        .is_none_or(|ops| ops.is_empty() || ops.contains(&op))
}

/// What a journal can report on this platform.
#[must_use]
pub(crate) fn capabilities(budget: usize) -> JournalCapabilities {
    JournalCapabilities {
        enabled: true,
        // Attribution is not guessable on any source we ship.
        writer_id: false,
        // inotify pairs the two halves of a rename; FSEvents and kqueue report
        // one path, so an origin is honestly absent there.
        rename_pairing: cfg!(target_os = "linux"),
        // We stat each change, so identity is available on the unices.
        file_identity: cfg!(unix),
        watch_budget: i32::try_from(budget).unwrap_or(i32::MAX),
    }
}

/// The journal of one runner.
pub(crate) struct Engine {
    ring: Arc<Mutex<Ring>>,
    source: Mutex<Option<source::NotifySource>>,
    capabilities: JournalCapabilities,
}

impl std::fmt::Debug for Engine {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("sandbox.Journal")
            .field("capabilities", &self.capabilities)
            .finish_non_exhaustive()
    }
}

impl Engine {
    /// What this journal reports.
    pub(crate) fn capabilities(&self) -> JournalCapabilities {
        self.capabilities
    }

    /// Open a journal on `root`.
    ///
    /// Fails when the platform cannot watch (no source, or the root is not
    /// watchable) rather than returning a journal that would silently report
    /// nothing.
    pub(crate) fn open(root: &Path, config: &Config) -> Result<Arc<Self>> {
        let (sink, mut receiver) = tokio::sync::mpsc::unbounded_channel::<Incoming>();
        let notify_source = source::NotifySource::start(root, config, sink)?;
        let ring = Arc::new(Mutex::new(Ring::new(config.resolved_retention())));
        let engine = Arc::new(Self {
            ring: ring.clone(),
            source: Mutex::new(Some(notify_source)),
            capabilities: capabilities(config.resolved_budget()),
        });

        let ops = config.ops.clone();
        let task_ring = ring.clone();
        // The wire contract says a path under the root is relative to it: an
        // absolute host path would leak the installation layout into a
        // model-facing stream.
        let root = root.to_path_buf();
        // The source is synchronous (a watcher thread); this task is the seam
        // between it and the ring.
        tokio::spawn(async move {
            let mut fold = Fold::new();
            while let Some(incoming) = receiver.recv().await {
                match incoming {
                    Incoming::Event(raw) => {
                        let Some(mut folded) = fold.apply(raw) else {
                            continue;
                        };
                        folded.path = relative(&root, &folded.path);
                        folded.old_path = folded.old_path.map(|path| relative(&root, &path));
                        if !allows(&ops, folded.op) {
                            // Filtered: never an event, so never a gap either.
                            continue;
                        }
                        let (dev, ino, size) = identity(Path::new(&folded.path));
                        if let Ok(mut ring) = task_ring.lock() {
                            ring.push(
                                folded.op,
                                folded.path,
                                folded.old_path,
                                folded.is_dir,
                                now_nanos(),
                                dev,
                                ino,
                                size,
                            );
                        }
                    }
                    Incoming::Gap(reason) => {
                        if let Ok(mut ring) = task_ring.lock() {
                            ring.gap(reason);
                        }
                    }
                }
            }
        });
        Ok(engine)
    }
}

fn lock(ring: &Arc<Mutex<Ring>>) -> Result<MutexGuard<'_, Ring>> {
    ring.lock()
        .map_err(|_| Error::internal("sandbox: journal lock poisoned"))
}

#[async_trait]
impl Journal for Engine {
    async fn read(&self, after_seq: u64, max: usize) -> Result<Batch> {
        Ok(lock(&self.ring)?.read(after_seq, max))
    }

    async fn close(&self) -> Result<()> {
        if let Ok(mut source) = self.source.lock() {
            // Dropping the watcher stops it; the bridge task then ends when its
            // channel closes.
            source.take();
        }
        lock(&self.ring)?.close();
        Ok(())
    }
}

/// Report `path` relative to `root` when it lives under it.
fn relative(root: &Path, path: &str) -> String {
    let path = Path::new(path);
    match path.strip_prefix(root) {
        Ok(inner) => inner.display().to_string(),
        Err(_) => path.display().to_string(),
    }
}

/// Filesystem identity for one path, when it still exists.
fn identity(path: &Path) -> (Option<u64>, Option<u64>, Option<i64>) {
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;
        match std::fs::symlink_metadata(path) {
            Ok(metadata) => (
                Some(metadata.dev()),
                Some(metadata.ino()),
                i64::try_from(metadata.size()).ok(),
            ),
            Err(_) => (None, None, None),
        }
    }
    #[cfg(not(unix))]
    {
        let _ = path;
        (None, None, None)
    }
}

/// Wall-clock nanoseconds since the epoch.
fn now_nanos() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |duration| duration.as_nanos() as u64)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_operation_round_trips_through_its_spelling() {
        for op in [
            FileOp::Create,
            FileOp::Write,
            FileOp::Rename,
            FileOp::Remove,
        ] {
            assert_eq!(FileOp::parse(op.as_str()).unwrap(), op);
        }
        assert!(FileOp::parse("chmod").is_err());
    }

    #[test]
    fn a_gap_reason_round_trips_through_its_spelling() {
        for reason in [
            GapReason::Overflow,
            GapReason::Retention,
            GapReason::Capacity,
            GapReason::WatchLost,
            GapReason::Backend,
        ] {
            assert_eq!(GapReason::parse(reason.as_str()).unwrap(), reason);
        }
        assert!(GapReason::parse("elsewhere").is_err());
    }

    #[test]
    fn settings_reject_malformed_values() {
        assert!(Config::parse(&serde_json::json!({"retention": 0})).is_err());
        assert!(Config::parse(&serde_json::json!({"max_watch_set": 0})).is_err());
        assert!(Config::parse(&serde_json::json!({"exclude": [""]})).is_err());
        assert!(Config::parse(&serde_json::json!({"unknown": true})).is_err());
        let config = Config::parse(&serde_json::json!({"retention": 8, "exclude": ["target"]}))
            .expect("settings");
        assert_eq!(config.resolved_retention(), 8);
        assert_eq!(config.resolved_budget(), DEFAULT_WATCH_BUDGET);
    }

    #[test]
    fn an_op_filter_is_a_filter_not_a_gap() {
        let config =
            Config::parse(&serde_json::json!({"ops": ["create", "rename"]})).expect("settings");
        assert!(allows(&config.ops, FileOp::Create));
        assert!(allows(&config.ops, FileOp::Rename));
        assert!(!allows(&config.ops, FileOp::Write));
        assert!(!allows(&config.ops, FileOp::Remove));
        // No filter keeps everything, and an empty list means "no filter" rather
        // than "nothing" — an empty `ops` is almost certainly a config slip.
        assert!(allows(&Config::default().ops, FileOp::Write));
        assert!(allows(
            &Config::parse(&serde_json::json!({"ops": []})).unwrap().ops,
            FileOp::Write
        ));
    }

    #[test]
    fn the_capability_table_matches_what_the_platform_can_do() {
        let capabilities = capabilities(4096);
        assert!(capabilities.enabled);
        assert!(!capabilities.writer_id, "attribution is never guessed");
        assert_eq!(capabilities.watch_budget, 4096);
        if cfg!(target_os = "linux") {
            assert!(capabilities.rename_pairing);
        }
    }

    #[tokio::test]
    async fn a_journal_over_a_temporary_tree_reports_net_changes() {
        let dir = tempfile::tempdir().expect("tempdir");
        let root = std::fs::canonicalize(dir.path()).expect("canonical");
        let config = Config::default();
        let journal = Engine::open(&root, &config).expect("journal");

        // A create that is then written must be one `create`.
        let created = root.join("artifact.txt");
        std::fs::write(&created, b"one").expect("create");
        tokio::time::sleep(std::time::Duration::from_millis(200)).await;
        std::fs::write(&created, b"two").expect("write");
        tokio::time::sleep(std::time::Duration::from_millis(200)).await;
        std::fs::remove_file(&created).expect("remove");
        tokio::time::sleep(std::time::Duration::from_millis(300)).await;

        let mut cursor = 0;
        let mut ops = Vec::new();
        for _ in 0..40 {
            let batch = journal.read(cursor, 64).await.expect("read");
            cursor = batch.next_seq;
            if let Some(gap) = batch.gap {
                panic!("unexpected gap: {gap:?}");
            }
            for event in batch.events {
                assert!(event.path.ends_with("artifact.txt"), "{event:?}");
                ops.push(event.op);
            }
            if ops.len() >= 2 {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(50)).await;
        }
        journal.close().await.expect("close");

        assert!(
            ops.contains(&FileOp::Create),
            "the creation must be reported: {ops:?}"
        );
        assert!(
            ops.contains(&FileOp::Remove),
            "the removal must be reported: {ops:?}"
        );
        let creates = ops.iter().filter(|op| **op == FileOp::Create).count();
        assert_eq!(creates, 1, "created and written is one create: {ops:?}");
    }

    #[tokio::test]
    async fn a_closed_journal_keeps_its_residue_readable() {
        let dir = tempfile::tempdir().expect("tempdir");
        let root = std::fs::canonicalize(dir.path()).expect("canonical");
        let journal = Engine::open(&root, &Config::default()).expect("journal");
        std::fs::write(root.join("f"), b"x").expect("write");
        tokio::time::sleep(std::time::Duration::from_millis(200)).await;
        journal.close().await.expect("close");
        let batch = journal.read(0, 64).await.expect("read");
        assert!(batch.closed, "the freeze is visible");
        assert!(!batch.events.is_empty(), "the residue stays readable");
    }

    #[test]
    fn an_unwatchable_root_fails_instead_of_reporting_nothing() {
        let journal = Engine::open(Path::new("/definitely/not/here"), &Config::default());
        assert!(journal.is_err(), "a journal that watches nothing is a lie");
    }
}
