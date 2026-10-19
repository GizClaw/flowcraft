//! `core/skill/discover.go`: the walk from configured roots to skills.
//!
//! A root is a directory that holds one skill or a tree of them, and discovery
//! is one breadth-first pass over every root. The rules that make the pass
//! finite and safe are the whole of the module:
//!
//! - Hidden entries are skipped, at every level.
//! - A symlink is followed only when its target, resolved once, stays inside
//!   one of the configured roots. A repository can therefore ship a link
//!   without redirecting discovery -- or a later body read -- outside the
//!   directories the deployment chose.
//! - Directories are tracked by canonical path, so an alias pointing back into
//!   a walked tree is not re-descended: the walk terminates on any topology,
//!   however it is linked.
//! - A skill reachable through several roots, or through a symlinked
//!   directory, is collected once, under its canonical path.
//! - A parse failure is a diagnostic, never a failed pass: one broken
//!   `SKILL.md` must not hide the rest of the tree.
//!
//! # What the port changed
//!
//! - Cancellation is a predicate rather than a `context.Context`: [`discover`]
//!   never cancels, [`discover_with`] takes the check the caller's runtime
//!   already has. Core checks `ctx.Err()` once per directory; the port checks
//!   the same way, so a cancelled pass stays partial in the same places.
//! - An operating system's sentence is the platform's, and the port wraps the
//!   same verb and path core wraps (`open <dir>: ...`, `stat <path>: ...`,
//!   `resolve symlink: lstat <path>: ...`) before appending its own text. The
//!   contract masks the operating system's half rather than pinning it.
//! - A path that is not valid UTF-8 is recorded lossily. Core keeps the raw
//!   bytes in a Go string; Rust's [`String`] cannot, and every consumer here
//!   (JSON, a prompt, a log line) needs one anyway. Nothing else about such a
//!   path differs -- the walk itself is on [`PathBuf`], so containment and
//!   de-duplication stay exact.

use std::collections::{HashSet, VecDeque};
use std::fs;
use std::io::ErrorKind as IoErrorKind;
use std::path::{Path, PathBuf};

use flowcraft_utils::pathsafe;

use crate::parse::{Metadata, parse_file};

/// A non-fatal discovery diagnostic: a parse failure, a tolerated validation
/// fallback, an escaping symlink, a directory that cannot be read.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Diagnostic {
    /// The path the diagnosis is about, as the walk spelled it.
    pub path: String,
    /// The sentence.
    pub message: String,
    /// Marks a tolerated shape issue (a third-party name that differs from
    /// its directory, for example) rather than a real error.
    pub warning: bool,
}

/// The result of one discovery pass.
#[derive(Debug, Clone, Default)]
pub struct Outcome {
    /// Every discovered skill, sorted by name and then by path.
    pub skills: Vec<Metadata>,
    /// The non-fatal issues of the pass, in walk order.
    pub diagnostics: Vec<Diagnostic>,
    /// Every configured root that contained at least one skill, in
    /// configuration order and de-duplicated: a root that resolves to an
    /// already-walked directory is listed once, the first time it is reached.
    pub roots: Vec<String>,
    /// Every configured root in absolute form, whether or not it contained a
    /// skill and whether or not the walk had to cover it again. The body
    /// readers use the list to keep symlinks from escaping the roots.
    pub scan_roots: Vec<String>,
}

/// Walks every root and collects each `SKILL.md` below it.
///
/// A missing root is silently empty; see the module documentation for the
/// rest of the rules.
pub fn discover(roots: &[String]) -> Outcome {
    discover_with(roots, &|| false)
}

/// [`discover`], stopping between directories when `cancelled` says so.
///
/// The outcome of a cancelled pass stays partial, exactly as core's does: the
/// skills and diagnostics collected so far are returned.
pub fn discover_with(roots: &[String], cancelled: &dyn Fn() -> bool) -> Outcome {
    let scans = absolute_roots(roots);
    let mut collector = Collector {
        cancelled,
        seen: HashSet::new(),
        seen_dirs: HashSet::new(),
        roots: cleaned_roots(&scans),
        skills: Vec::new(),
        diagnostics: Vec::new(),
    };
    let mut found = Vec::new();
    for root in &scans {
        // Walk the canonical form of a root, and walk a directory at most
        // once: a root configured twice -- or a second root that resolves to
        // a directory the walk already covered -- is not walked or listed
        // again.
        let walk = resolve_root(Path::new(root));
        if !collector.seen_dirs.insert(walk.clone()) {
            continue;
        }
        if collector.scan_root(&walk) {
            found.push(root.clone());
        }
    }
    let mut outcome = Outcome {
        skills: collector.skills,
        diagnostics: collector.diagnostics,
        roots: found,
        scan_roots: scans,
    };
    outcome.skills.sort_by(|left, right| {
        left.name
            .cmp(&right.name)
            .then_with(|| left.path.cmp(&right.path))
    });
    outcome
}

/// Normalizes configured roots to absolute cleaned paths, dropping empty
/// entries. A root that cannot be made absolute (a deleted working directory,
/// say) is kept cleaned as-is.
fn absolute_roots(roots: &[String]) -> Vec<String> {
    let mut out = Vec::with_capacity(roots.len());
    for root in roots {
        if root.is_empty() {
            continue;
        }
        let path = match std::env::current_dir() {
            Ok(cwd) if !Path::new(root).is_absolute() => cwd.join(root),
            _ => PathBuf::from(root),
        };
        out.push(path_string(&pathsafe::clean(&path)));
    }
    out
}

/// The canonical form of one root, so containment checks and walk bookkeeping
/// compare real paths (macOS `/var` is a symlink to `/private/var`, for
/// example). A root that cannot be resolved -- a missing directory, say --
/// stays cleaned; walking it is quietly empty.
fn resolve_root(root: &Path) -> PathBuf {
    let clean = pathsafe::clean(root);
    fs::canonicalize(&clean).unwrap_or(clean)
}

/// The resolved form of each root, as strings.
fn cleaned_roots(roots: &[String]) -> Vec<String> {
    roots
        .iter()
        .filter(|root| !root.is_empty())
        .map(|root| path_string(&resolve_root(Path::new(root))))
        .collect()
}

/// Whether `resolved` stays inside one of `roots`. Callers resolve symlinks
/// first; the comparison is lexical.
pub(crate) fn inside_any_root(resolved: &Path, roots: &[String]) -> bool {
    roots
        .iter()
        .any(|root| pathsafe::within(root.as_str(), resolved))
}

/// One path as a record: lossy for a non-UTF-8 name, which is all a consumer
/// of a diagnostic or a metadata record can carry anyway.
fn path_string(path: &Path) -> String {
    path.to_string_lossy().into_owned()
}

/// The state of one pass.
struct Collector<'a> {
    /// The caller's cancellation check.
    cancelled: &'a dyn Fn() -> bool,
    /// Canonical `SKILL.md` paths, collected once.
    seen: HashSet<PathBuf>,
    /// Canonical directories, walked once.
    seen_dirs: HashSet<PathBuf>,
    /// Resolved scan roots, for symlink containment.
    roots: Vec<String>,
    /// What the pass collected.
    skills: Vec<Metadata>,
    /// What the pass noticed.
    diagnostics: Vec<Diagnostic>,
}

impl Collector<'_> {
    /// Records one diagnostic.
    fn report(&mut self, path: &Path, message: String, warning: bool) {
        self.diagnostics.push(Diagnostic {
            path: path_string(path),
            message,
            warning,
        });
    }

    /// Walks one root, collecting every `SKILL.md` below it and reporting what
    /// it could not read. Returns whether the walk collected at least one
    /// skill.
    fn scan_root(&mut self, root: &Path) -> bool {
        let before = self.skills.len();
        let mut queue: VecDeque<PathBuf> = VecDeque::new();
        queue.push_back(root.to_path_buf());
        while let Some(dir) = queue.pop_front() {
            if (self.cancelled)() {
                break;
            }
            let read = match fs::read_dir(&dir) {
                Ok(read) => read,
                Err(err) => {
                    if err.kind() != IoErrorKind::NotFound {
                        self.report(&dir, format!("open {}: {err}", dir.display()), false);
                    }
                    continue;
                }
            };
            // `os.ReadDir` sorts by name, and the port has to walk in the same
            // order or its diagnostics would be emitted in another one.
            let mut entries: Vec<fs::DirEntry> = read.filter_map(Result::ok).collect();
            entries.sort_by(|left, right| {
                left.file_name()
                    .as_encoded_bytes()
                    .cmp(right.file_name().as_encoded_bytes())
            });
            for entry in entries {
                let name = entry.file_name();
                if name.as_encoded_bytes().starts_with(b".") {
                    continue;
                }
                let full = dir.join(&name);
                let mut target = full.clone();
                if entry.file_type().is_ok_and(|kind| kind.is_symlink()) {
                    // Resolve once and verify the target stays inside the
                    // configured roots: a repository-supplied symlink must
                    // never redirect discovery outside them.
                    match fs::canonicalize(&full) {
                        Ok(resolved) => {
                            if !inside_any_root(&resolved, &self.roots) {
                                self.report(
                                    &full,
                                    "symlink escapes the configured skill roots".to_owned(),
                                    false,
                                );
                                continue;
                            }
                            target = resolved;
                        }
                        Err(err) => {
                            self.report(
                                &full,
                                format!("resolve symlink: lstat {}: {err}", full.display()),
                                false,
                            );
                            continue;
                        }
                    }
                }
                // A vanished entry, or one the kernel refuses (ELOOP,
                // ENAMETOOLONG): reporting it keeps a walk that ends early from
                // doing so silently.
                let info = match fs::metadata(&target) {
                    Ok(info) => info,
                    Err(err) => {
                        self.report(&full, format!("stat {}: {err}", full.display()), false);
                        continue;
                    }
                };
                if info.is_dir() {
                    // Tracked by canonical path: an alias pointing back into a
                    // walked tree (or at another root) is walked once, which
                    // keeps the walk finite.
                    if self.seen_dirs.insert(target.clone()) {
                        queue.push_back(target);
                    }
                    continue;
                }
                if name.as_encoded_bytes() != b"SKILL.md" {
                    continue;
                }
                // Canonicalized so a skill reachable through several roots (or
                // through a symlinked directory) is collected once, under the
                // path its readers resolve to.
                if self.seen.contains(&target) {
                    continue;
                }
                match parse_file(&target) {
                    Ok(parsed) => {
                        for warning in parsed.warnings {
                            self.report(&full, warning, true);
                        }
                        self.seen.insert(target);
                        self.skills.push(parsed.metadata);
                    }
                    Err(err) => self.report(&full, err.to_string(), false),
                }
            }
        }
        self.skills.len() > before
    }
}
