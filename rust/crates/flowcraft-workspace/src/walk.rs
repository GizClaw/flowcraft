//! [`walk`] and [`glob`]: traversal written in terms of [`Workspace::list`],
//! and the `**` layer over Go's `path.Match`.
//!
//! Neither knows what a filesystem is. `walk` recurses on `list` and hands every
//! entry to a callback; `glob` walks from the root and keeps the non-directory
//! entries whose path a pattern matches. That is why these live here rather than
//! on a backend: an object store with a native directory concept gets the same
//! order as a local tree.
//!
//! # Order is part of the answer
//!
//! `list` is sorted by name, and `walk` is depth-first in that order, visiting a
//! directory before its contents. A caller that enumerates a tree -- rendering a
//! file index into a prompt, deciding which ancestor to blame -- gets the same
//! sequence from both implementations, so the sequence is in the contract rather
//! than left to the medium.
//!
//! # `**` is not `path.Match`
//!
//! Go's `path.Match` has no `**`. core adds it by splitting the pattern on `/`
//! and letting one segment `**` match any number of path segments, including
//! none -- so `**/*.go` matches a `.go` file at the root, and `a/**/b` matches
//! `a/b`. The split is a *string* test for `"**"`: any pattern containing two
//! adjacent stars goes down the segment path, which is why `a**b` becomes one
//! segment containing `**` and matches nothing (a `**` segment only spans whole
//! segments). The port keeps that, including the fact that a malformed segment
//! is *ignored* on this path -- `matchParts` drops `path.Match`'s error -- while
//! a malformed pattern without `**` aborts the walk. Both directions have a row.

use flowcraft_utils::glob::path_match;

use crate::clean::{clean, fold_case, join};
use crate::{Entry, Result, Workspace};

/// What a [`walk`] callback answers.
#[derive(Debug)]
pub enum Step {
    /// Visit the entry's children, if it is a directory.
    Continue,
    /// Skip this entry's subtree. Not an error, even for a file.
    Skip,
    /// Abandon the walk with this refusal.
    Fail(flowcraft_contract::error::Error),
}

/// Calls `visit` for every entry under `dir`, depth-first and in `list` order.
///
/// A directory is visited before its contents. The paths handed to `visit` are
/// slash-separated and relative to the view's root: `walk(ws, ".", ..)` answers
/// `a`, `a/b`, not `./a`.
pub fn walk(
    ws: &dyn Workspace,
    dir: &str,
    visit: &mut dyn FnMut(&str, &Entry) -> Step,
) -> Result<()> {
    let entries = ws.list(dir)?;
    for entry in entries {
        let child = join(&[dir, &entry.name]);
        match visit(&child, &entry) {
            Step::Continue => {}
            Step::Skip => continue,
            Step::Fail(err) => return Err(err),
        }
        if entry.is_dir {
            walk(ws, &child, visit)?;
        }
    }
    Ok(())
}

/// A [`glob`] answer: the matches found, and the refusal that stopped the walk.
///
/// Both can be non-empty. core's `Glob` returns `(matches, err)` and leaves the
/// caller to decide what a partial answer means; the port does the same rather
/// than throwing the partial list away, and [`glob`] is the `Result`-shaped view
/// over it for callers that do not care.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct GlobalMatch {
    /// The matching paths, in walk order.
    pub matches: Vec<String>,
    /// Go's `syntax error in pattern`, when the pattern was malformed.
    pub error: Option<String>,
}

/// Answers the paths under the root matching `pattern`.
///
/// `*` and `?` never cross a `/`. A `**` segment matches any number of directory
/// levels, including none. A pattern with no `**` is matched with
/// [`flowcraft_utils::glob::path_match`] against the whole path, and a malformed
/// one aborts the walk with Go's `syntax error in pattern`; a malformed segment
/// of a `**` pattern simply does not match (see the module docs).
pub fn glob(ws: &dyn Workspace, pattern: &str) -> Result<Vec<String>> {
    let found = glob_detailed(ws, pattern)?;
    match found.error {
        Some(message) => Err(crate::refusal::validation(message)),
        None => Ok(found.matches),
    }
}

/// [`glob`] without the refusal-as-error step: the matches and the error text,
/// which is what the conformance dump records for a bad pattern.
pub fn glob_detailed(ws: &dyn Workspace, pattern: &str) -> Result<GlobalMatch> {
    let has_doublestar = contains_doublestar(pattern);
    let pattern = fold_case(pattern);

    let mut found = GlobalMatch {
        matches: Vec::new(),
        error: None,
    };
    let outcome = walk(ws, ".", &mut |path, entry| {
        if entry.is_dir {
            return Step::Continue;
        }
        let matched = if has_doublestar {
            Ok(match_doublestar(&pattern, &fold_case(path)))
        } else {
            path_match(&pattern, &fold_case(path)).map_err(|e| e.to_string())
        };
        match matched {
            Ok(true) => {
                found.matches.push(path.to_string());
                Step::Continue
            }
            Ok(false) => Step::Continue,
            // core returns the error out of the callback, which aborts the
            // walk; the matches found before it are returned alongside.
            Err(message) => Step::Fail(crate::refusal::validation(message)),
        }
    });
    match outcome {
        Ok(()) => Ok(found),
        Err(err) => {
            found.error = Some(err.to_string());
            Ok(found)
        }
    }
}

/// Whether the pattern contains two adjacent stars anywhere.
fn contains_doublestar(pattern: &str) -> bool {
    pattern.as_bytes().windows(2).any(|w| w == b"**")
}

/// `**`-aware matching: split both sides into segments and let `**` span.
fn match_doublestar(pattern: &str, path: &str) -> bool {
    match_parts(&split_path(pattern), &split_path(path))
}

/// Splits a slash-separated path into its components, as core's `split` does.
///
/// Not `split('/')`: the path is cleaned first, so `a//b/` and `a/b` give the
/// same segments and `.` gives none.
fn split_path(path: &str) -> Vec<String> {
    let cleaned = clean(path);
    if cleaned == "." || cleaned == "/" {
        return Vec::new();
    }
    cleaned
        .split('/')
        .filter(|part| !part.is_empty())
        .map(|part| part.to_string())
        .collect()
}

/// Matches one pattern segment list against one path segment list.
fn match_parts(pattern: &[String], path: &[String]) -> bool {
    if pattern.is_empty() {
        return path.is_empty();
    }
    if pattern[0] == "**" {
        let rest = &pattern[1..];
        if rest.is_empty() {
            return true;
        }
        return (0..=path.len()).any(|i| match_parts(rest, &path[i..]));
    }
    if path.is_empty() {
        return false;
    }
    let matched = path_match(&pattern[0], &path[0]).unwrap_or(false);
    matched && match_parts(&pattern[1..], &path[1..])
}
