//! `core/skill/service.go`: the shared registry over one discovery pass.
//!
//! A [`Service`] is one discovery pass, one BM25 index over skill names and
//! descriptions, and read access to the `SKILL.md` bodies. The registry is an
//! immutable snapshot behind a lock, so [`Service::reload`] swaps in a freshly
//! discovered registry while per-turn reads (ranking, mention resolution,
//! rendering) keep working on the one they started with.
//!
//! Shadowing is the caller's policy. Duplicate names across roots are all
//! listed and rankable; [`Service::by_name`] and [`Service::mentioned`] resolve
//! the first path in the canonical (name, path) order.
//!
//! # What the port changed
//!
//! - `context.Context` is not carried. Discovery's cancellation check is an
//!   argument to [`Service::reload_with`], because the port has no telemetry or
//!   runtime context to own one yet; [`Service::new`] never cancels. Core's
//!   per-call `ctx` was only ever used for discovery and one warning, so
//!   nothing observable moves.
//! - Finding out that the index cannot be built degrades the registry to
//!   unranked, as core does, but the port has no telemetry sink to log the
//!   warning to yet. Discovery de-duplicates canonical paths, and `bm25` only
//!   refuses duplicate ids, so the path is unreachable in practice; it exists
//!   so that a future rule cannot turn "no ranking" into "no skills".
//! - The snapshot is an [`RwLock`] over an [`Arc`] rather than an
//!   `atomic.Pointer`. Readers clone the pointer under a read lock, which is
//!   the same guarantee -- an immutable snapshot, no locking on the read path
//!   beyond that clone -- without adding a dependency to the crate.
//! - Error kinds are named: a missing skill or path is [`ErrorKind::NotFound`]
//!   and every filesystem refusal is [`ErrorKind::Validation`], where core
//!   returns them unclassified. The sentences are core's.

use std::collections::BTreeMap;
use std::fs;
use std::path::Path;
use std::sync::{Arc, PoisonError, RwLock};

use flowcraft_contract::error::{Error, ErrorKind};
use flowcraft_utils::bm25;
use flowcraft_utils::pathsafe;

use crate::discover::{Outcome, discover, discover_with, inside_any_root};
use crate::parse::{MAX_SKILL_FILE_BYTES, Metadata, split_frontmatter};

/// The cap on a ranked list when neither the call nor [`Options`] sets one.
pub const DEFAULT_TOP_N: usize = 5;

/// Configures a [`Service`].
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Options {
    /// The directories discovery scans (see [`crate::discover`]). The
    /// conventional personal root `~/.agents/skills` is deliberately not
    /// implicit: the deployment composes its roots.
    pub roots: Vec<String>,
    /// Caps ranked results when a call passes no limit of its own. Zero means
    /// [`DEFAULT_TOP_N`]; core spells the same rule as `<= 0` on a signed
    /// integer.
    pub top_n: usize,
    /// The default BM25 acceptance threshold. Zero accepts any match.
    pub min_score: f64,
    /// Skill names, or absolute `SKILL.md` paths from the roots, excluded from
    /// the registry and therefore from ranking.
    pub disabled: Vec<String>,
}

/// A shared skills registry: one discovery pass, one index, one snapshot.
pub struct Service {
    /// The configuration the service was built with.
    opts: Options,
    /// The current immutable snapshot.
    snapshot: RwLock<Arc<Snapshot>>,
}

/// One immutable discovery result.
pub(crate) struct Snapshot {
    /// What discovery found, after [`Options::disabled`] was applied.
    pub(crate) outcome: Outcome,
    /// The same skills, keyed by canonical path.
    pub(crate) by_path: BTreeMap<String, Metadata>,
    /// The ranking index; absent only if `bm25` refused the documents.
    pub(crate) index: Option<bm25::Index>,
    /// Symlink-resolved roots, for the body-read containment check.
    pub(crate) scan_roots: Vec<String>,
}

impl Service {
    /// Discovers every root and builds the shared index.
    ///
    /// Roots are scanned once here and again on every
    /// [`reload`](Service::reload).
    pub fn new(opts: Options) -> Service {
        let service = Service {
            opts: normalise(opts),
            snapshot: RwLock::new(Arc::new(Snapshot::empty())),
        };
        service.reload();
        service
    }

    /// Re-runs discovery and swaps in a fresh snapshot, so skills added on
    /// disk become visible without rebuilding the service.
    pub fn reload(&self) {
        self.reload_with(&|| false);
    }

    /// [`reload`](Service::reload), stopping between directories when
    /// `cancelled` says so. A cancelled pass publishes what it collected.
    pub fn reload_with(&self, cancelled: &dyn Fn() -> bool) {
        let mut outcome = discover_with(&self.opts.roots, cancelled);
        outcome.skills = filter_disabled(&outcome.skills, &self.opts.disabled);
        let mut by_path = BTreeMap::new();
        let mut docs = Vec::with_capacity(outcome.skills.len());
        for skill in &outcome.skills {
            by_path.insert(skill.path.clone(), skill.clone());
            docs.push(bm25::Doc::new(
                skill.path.clone(),
                skill.name.clone(),
                skill.description.clone(),
            ));
        }
        // Skills arrive sorted by (name, path) and the index keeps the document
        // order for score ties, so equal scores stay stable.
        let index = bm25::Index::new(&docs, bm25::Options::default()).ok();
        let scan_roots = cleaned_roots(&outcome.scan_roots);
        let snapshot = Snapshot {
            outcome,
            by_path,
            index,
            scan_roots,
        };
        self.store(snapshot);
    }

    /// The configured ranked-list size.
    pub fn top_n(&self) -> usize {
        self.opts.top_n
    }

    /// The configured BM25 threshold.
    pub fn min_score(&self) -> f64 {
        self.opts.min_score
    }

    /// The configuration the service was built with, normalized.
    pub fn options(&self) -> &Options {
        &self.opts
    }

    /// Every discovered skill, sorted by name and then by path.
    ///
    /// Unfiltered beyond [`Options::disabled`]: shadowing and lifecycle
    /// decisions belong to the caller, which sees every path.
    pub fn list(&self) -> Vec<Metadata> {
        let snapshot = self.snapshot();
        let mut out = snapshot.outcome.skills.clone();
        out.sort_by(|left, right| {
            left.name
                .cmp(&right.name)
                .then_with(|| left.path.cmp(&right.path))
        });
        out
    }

    /// The non-fatal diagnostics of the last discovery.
    pub fn diagnostics(&self) -> Vec<crate::discover::Diagnostic> {
        self.snapshot().outcome.diagnostics.clone()
    }

    /// The scan roots that contained at least one skill.
    pub fn roots(&self) -> Vec<String> {
        self.snapshot().outcome.roots.clone()
    }

    /// Every configured root in absolute form, whether or not it was walked.
    pub fn scan_roots(&self) -> Vec<String> {
        self.snapshot().outcome.scan_roots.clone()
    }

    /// Resolves a skill by name: the first path in the canonical (name, path)
    /// order, the same order [`list`](Service::list) returns.
    pub fn by_name(&self, name: &str) -> Option<Metadata> {
        let snapshot = self.snapshot();
        snapshot
            .outcome
            .skills
            .iter()
            .find(|skill| skill.name == name)
            .cloned()
    }

    /// A skill's metadata plus its full `SKILL.md` body, with the frontmatter
    /// stripped (the activation and `skill_read` paths share this).
    pub fn read_full(&self, name: &str) -> Result<(Metadata, String), Error> {
        let skill = self.by_name(name).ok_or_else(|| {
            Error::new(
                ErrorKind::NotFound,
                format!("skill: {} not found", quote(name)),
            )
        })?;
        let body = self.read_body(&skill)?;
        Ok((skill, body))
    }

    /// The metadata plus full body for one discovered skill, identified by the
    /// exact path [`list`](Service::list) returned.
    ///
    /// Like [`read_full`](Service::read_full) it serves only paths in the
    /// current snapshot, so a stale path from a UI cannot read an arbitrary
    /// file on disk.
    pub fn read_by_path(&self, path: &str) -> Result<(Metadata, String), Error> {
        let snapshot = self.snapshot();
        let skill = snapshot.by_path.get(path).cloned().ok_or_else(|| {
            Error::new(
                ErrorKind::NotFound,
                format!("skill: {} not found", quote(path)),
            )
        })?;
        let body = self.read_body(&skill)?;
        Ok((skill, body))
    }

    /// Re-reads one `SKILL.md` through the snapshot's roots: the file is
    /// resolved again, refused when it left the roots after discovery,
    /// size-capped again, and returned with its frontmatter stripped.
    ///
    /// The read opens the resolved path, not the discovered one, so only a
    /// swap of the resolved file itself can race the containment check --
    /// accepted: the roots are trusted content directories, and a writer able
    /// to swap files inside them already decides what the model reads.
    fn read_body(&self, skill: &Metadata) -> Result<String, Error> {
        let snapshot = self.snapshot();
        let resolved = fs::canonicalize(&skill.path).map_err(|err| {
            Error::new(
                ErrorKind::Validation,
                format!("skill: resolve {}: {err}", skill.path),
            )
        })?;
        if !inside_any_root(&resolved, &snapshot.scan_roots) {
            return Err(Error::new(
                ErrorKind::Validation,
                format!(
                    "skill: {} resolves outside the configured skill roots",
                    quote(&skill.path)
                ),
            ));
        }
        let data = fs::read(&resolved).map_err(|err| {
            Error::new(
                ErrorKind::Validation,
                format!("skill: read {}: {err}", skill.path),
            )
        })?;
        if data.len() as u64 > MAX_SKILL_FILE_BYTES {
            return Err(Error::new(
                ErrorKind::Validation,
                format!(
                    "skill: {} exceeds the {MAX_SKILL_FILE_BYTES}-byte SKILL.md limit",
                    skill.path
                ),
            ));
        }
        let parts = split_frontmatter(&data).map_err(|err| {
            Error::new(
                ErrorKind::Validation,
                format!("skill: {}: {err}", skill.path),
            )
        })?;
        Ok(String::from_utf8_lossy(parts.body).trim().to_owned())
    }

    /// The current snapshot, cloned out of the lock.
    pub(crate) fn snapshot(&self) -> Arc<Snapshot> {
        let guard = self.snapshot.read().unwrap_or_else(PoisonError::into_inner);
        Arc::clone(&guard)
    }

    /// Replaces the snapshot.
    fn store(&self, snapshot: Snapshot) {
        let mut guard = self
            .snapshot
            .write()
            .unwrap_or_else(PoisonError::into_inner);
        *guard = Arc::new(snapshot);
    }
}

impl Snapshot {
    /// The snapshot a service starts with, before its first pass.
    fn empty() -> Snapshot {
        Snapshot {
            outcome: discover(&[]),
            by_path: BTreeMap::new(),
            index: None,
            scan_roots: Vec::new(),
        }
    }
}

/// Applies the two rules [`Options::top_n`] states.
fn normalise(opts: Options) -> Options {
    Options {
        top_n: if opts.top_n == 0 {
            DEFAULT_TOP_N
        } else {
            opts.top_n
        },
        ..opts
    }
}

/// Returns the skills that survive the disabled list: an absolute path matches
/// the canonical path exactly, anything else matches the name. The input slice
/// is left untouched -- callers keep it in the snapshot.
fn filter_disabled(skills: &[Metadata], disabled: &[String]) -> Vec<Metadata> {
    if disabled.is_empty() {
        return skills.to_vec();
    }
    let mut names = Vec::new();
    let mut paths = Vec::new();
    for entry in disabled {
        let entry = entry.trim();
        if entry.is_empty() {
            continue;
        }
        if Path::new(entry).is_absolute() {
            paths.push(pathsafe::clean(entry));
        } else {
            names.push(entry);
        }
    }
    if names.is_empty() && paths.is_empty() {
        return skills.to_vec();
    }
    skills
        .iter()
        .filter(|skill| {
            !names.contains(&skill.name.as_str())
                && !paths.contains(&pathsafe::clean(skill.path.as_str()))
        })
        .cloned()
        .collect()
}

/// The resolved form of each root, as strings.
fn cleaned_roots(roots: &[String]) -> Vec<String> {
    roots
        .iter()
        .filter(|root| !root.is_empty())
        .map(|root| {
            let clean = pathsafe::clean(root.as_str());
            match fs::canonicalize(&clean) {
                Ok(resolved) => resolved.to_string_lossy().into_owned(),
                Err(_) => clean.to_string_lossy().into_owned(),
            }
        })
        .collect()
}

/// Go's `%q` on a string, for the sentences core builds with it: quoted, with
/// the escapes core would use for the common cases. A path with a control
/// character is the one shape the two spellings can differ on.
fn quote(text: &str) -> String {
    let mut out = String::with_capacity(text.len() + 2);
    out.push('"');
    for ch in text.chars() {
        match ch {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\t' => out.push_str("\\t"),
            '\r' => out.push_str("\\r"),
            ch => out.push(ch),
        }
    }
    out.push('"');
    out
}
