//! One spelling of "which paths a rule names".
//!
//! Two callers share it: [`crate::helpers::glob`], where a pattern selects files
//! to return, and [`crate::scoped`], where a pattern selects the paths a policy
//! lets through. Sharing the compiler is what keeps a deny rule and a glob
//! search from disagreeing about what `**` means.
//!
//! Rules that are deliberate and differ from a naive glob:
//!
//! * `*` and `?` never cross a `/`; only `**` spans components. So `*.ext`
//!   matches a top-level file only, and `**/*.ext` is how a caller says "at any
//!   depth".
//! * a bare `**` matches everything, including nested paths. The Go side's
//!   hand-rolled matcher instead falls through to `path.Match("**", path)`,
//!   which cannot cross a `/`, so the same policy denies `a/b.txt` there. The
//!   Rust reading is the intended one — `allow_write: ["**"]` is the documented
//!   way to say "writes are allowed" — and the divergence is pinned by a test.
//! * a pattern ending in `/**` also names the directory itself, because a policy
//!   rule that says "everything under `secrets`" must not be sidestepped by
//!   addressing `secrets`. This expansion applies to [`compile_set`] (policy
//!   rules); [`compile`] (a glob search) returns files only, so it needs none.
//!
//! Case is folded on Windows only, matching the Go side. That is narrower than
//! "wherever the filesystem folds case": a case-insensitive host elsewhere
//! (default APFS on macOS, say) is not folded, because the Go side does not fold
//! it either and a policy that behaved differently per host would be worse than
//! one that is merely conservative.
//!
//! Callers match the **cleaned** path (`crate::path::clean`), never the raw
//! string, so `a/../b` is matched as `b` — the path the provider will act on.

use flowcraft_error::{Error, Result};
use globset::{GlobBuilder, GlobMatcher, GlobSet, GlobSetBuilder};

/// Compile one pattern, for matching a single path at a time.
pub(crate) fn compile(pattern: &str) -> Result<GlobMatcher> {
    Ok(builder(pattern)?.compile_matcher())
}

/// Compile a policy's patterns into one set, expanding the `/**` self-match.
pub(crate) fn compile_set(patterns: &[String]) -> Result<GlobSet> {
    let mut set = GlobSetBuilder::new();
    for pattern in patterns {
        for expanded in expand(pattern) {
            set.add(builder(&expanded)?);
        }
    }
    set.build()
        .map_err(|error| invalid(&patterns.join(", "), &error))
}

fn builder(pattern: &str) -> Result<globset::Glob> {
    GlobBuilder::new(pattern)
        .literal_separator(true)
        .backslash_escape(false)
        .case_insensitive(cfg!(windows))
        .build()
        .map_err(|error| invalid(pattern, &error))
}

/// `dir/**` matches `dir` too, so "everything under `dir`" cannot be sidestepped
/// by naming `dir`.
fn expand(pattern: &str) -> Vec<String> {
    match pattern.strip_suffix("/**") {
        Some(prefix) if !prefix.is_empty() => vec![pattern.to_owned(), prefix.to_owned()],
        _ => vec![pattern.to_owned()],
    }
}

fn invalid(pattern: &str, error: &globset::Error) -> Error {
    Error::validation(format!("workspace: invalid pattern {pattern:?}: {error}"))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn matches(pattern: &str, path: &str) -> bool {
        compile(pattern).unwrap().is_match(path)
    }

    #[test]
    fn a_star_stays_inside_one_component() {
        assert!(matches("*.json", "top.json"));
        assert!(!matches("*.json", "nested/deep.json"));
        assert!(matches("**/*.json", "nested/deep.json"));
        assert!(matches("**/*.json", "top.json"));
    }

    /// The Rust reading of a bare `**` (see the module docs): it spans
    /// components, unlike the Go side's fall-through to `path.Match`.
    #[test]
    fn a_bare_double_star_matches_everything() {
        assert!(matches("**", "a"));
        assert!(matches("**", "a/b/c.txt"));
        let set = compile_set(&["**".to_owned()]).unwrap();
        assert!(set.is_match("a/b.txt"));
    }

    #[test]
    fn a_trailing_double_star_also_names_the_directory() {
        let set = compile_set(&["secrets/**".to_owned()]).unwrap();
        assert!(set.is_match("secrets"));
        assert!(set.is_match("secrets/key"));
        assert!(!set.is_match("secrets2/key"));

        let nested = compile_set(&["**/node_modules/**".to_owned()]).unwrap();
        assert!(nested.is_match("node_modules"));
        assert!(nested.is_match("a/node_modules"));
        assert!(nested.is_match("a/node_modules/pkg/index.js"));
    }

    #[test]
    fn an_invalid_pattern_is_a_validation_error() {
        assert_eq!(
            compile("a[b").unwrap_err().kind(),
            flowcraft_error::ErrorKind::Validation
        );
    }

    /// Only runs on Windows: matching folds case exactly where the filesystem
    /// does, so a deny rule cannot be sidestepped by spelling the path
    /// differently.
    #[cfg(windows)]
    #[test]
    fn matching_folds_case_where_the_filesystem_does() {
        let set = compile_set(&["Secrets/**".to_owned()]).unwrap();
        assert!(set.is_match("secrets/key"));
        assert!(set.is_match("SECRETS"));
        assert!(matches("**/*.JSON", "a/b.json"));
    }
}
