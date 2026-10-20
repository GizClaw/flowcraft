//! [`matches_any`]: the pattern language a [`crate::Scoped`] view decides with.
//!
//! Four forms, and they are *not* one grammar -- each is a separate string rule
//! evaluated in order, which is why the same-looking patterns differ:
//!
//! | pattern | matches |
//! |---|---|
//! | `dir/**` | `dir` itself and everything under it |
//! | `**/dir/**` | `dir` at any depth and everything under it |
//! | `**/name` | `name` at any depth, as a file or a directory |
//! | `*.ext` | only in the root, because there is no separator in the pattern |
//!
//! A pattern with no separator is matched against the *whole* path with
//! [`path_match`], so `*.ext` cannot reach `a/b.ext` -- the docs in core say this
//! and the rows hold it. To reach any depth, spell `**/*.ext`, which takes the
//! `**/` branch and compares by suffix (and, when the rest carries a wildcard,
//! against the path's base name as well).
//!
//! Two more properties a reader should not have to guess:
//!
//! - **A malformed pattern matches nothing.** Every `path.Match` error is
//!   dropped: a policy file with a typo in a deny list fails *open* here, not
//!   closed. (core's `ScopedWorkspace` is an allow-list for writes, so a typo in
//!   `allow_write` fails closed; a typo in `deny_read` does not.) The port keeps
//!   that, because changing it would change which files an existing deployment
//!   can read.
//! - **Case is folded when the target folds it**, which is never, on the platforms
//!   this crate is built for: see [`crate::fold_case`].

use flowcraft_utils::glob::path_match;

use crate::clean::{base, fold_case};

/// Reports whether `path` matches any of `patterns`.
///
/// The order of the four forms is core's, and the first match wins.
pub fn matches_any(path: &str, patterns: &[String]) -> bool {
    let path = fold_case(path);
    for pattern in patterns {
        let pattern = fold_case(pattern);

        if let Some(prefix) = pattern.strip_suffix("/**") {
            if let Some(dir) = prefix.strip_prefix("**/") {
                // `**/dir/**`: at any depth, and `dir` itself.
                let nested = format!("/{dir}/");
                if path == dir
                    || path.starts_with(&format!("{dir}/"))
                    || path.contains(&nested)
                    || path.ends_with(&format!("/{dir}"))
                {
                    return true;
                }
                continue;
            }
            // `dir/**`: `dir` itself and everything under it.
            if path == prefix || path.starts_with(&format!("{prefix}/")) {
                return true;
            }
            continue;
        }

        if let Some(suffix) = pattern.strip_prefix("**/") {
            if path == suffix || path.ends_with(&format!("/{suffix}")) {
                return true;
            }
            if (suffix.contains('*') || suffix.contains('?'))
                && path_match(suffix, &base(&path)).unwrap_or(false)
            {
                return true;
            }
            continue;
        }

        if path_match(&pattern, &path).unwrap_or(false) {
            return true;
        }
    }
    false
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hit(pattern: &str, path: &str) -> bool {
        matches_any(path, &[pattern.to_string()])
    }

    #[test]
    fn a_trailing_doublestar_covers_the_directory_and_its_contents() {
        assert!(hit("dir/**", "dir"));
        assert!(hit("dir/**", "dir/a"));
        assert!(hit("dir/**", "dir/a/b"));
        assert!(!hit("dir/**", "xdir"));
        assert!(!hit("dir/**", "xdir/a"));
        assert!(!hit("dir/**", "other/dir/a"));
    }

    #[test]
    fn a_leading_doublestar_covers_every_depth() {
        assert!(hit("**/dir/**", "dir"));
        assert!(hit("**/dir/**", "dir/a"));
        assert!(hit("**/dir/**", "a/dir"));
        assert!(hit("**/dir/**", "a/dir/x/y"));
        assert!(hit("**/dir/**", "a/b/dir/c"));
        assert!(!hit("**/dir/**", "adir"));
        assert!(!hit("**/dir/**", "a/dirx"));
        assert!(!hit("**/dir/**", "a/dirx/b"));
    }

    #[test]
    fn a_bare_leading_doublestar_is_a_suffix_rule() {
        assert!(hit("**/name", "name"));
        assert!(hit("**/name", "a/name"));
        assert!(hit("**/name", "a/b/name"));
        assert!(!hit("**/name", "name/x"));
        assert!(!hit("**/name", "a/namex"));
        // With a wildcard left in the suffix it also matches the base name, so
        // `**/*.env` reaches any depth.
        assert!(hit("**/*.env", ".env"));
        assert!(hit("**/*.env", "a/b/.env"));
        assert!(!hit("**/*.env", "a/b/.envx"));
    }

    #[test]
    fn a_pattern_without_a_separator_only_reaches_the_root() {
        assert!(hit("*.ext", "a.ext"));
        assert!(!hit("*.ext", "d/a.ext"));
        assert!(hit("*", "a"));
        assert!(!hit("*", "d/a"));
        assert!(hit("a/b", "a/b"));
        assert!(!hit("a/b", "a/b/c"));
    }

    #[test]
    fn a_malformed_pattern_matches_nothing() {
        assert!(!hit("a[", "a"));
        assert!(!hit("**/a[", "a"));
        assert!(!hit("[a-", "ab"));
        // ... and does not stop the list from being read further.
        assert!(matches_any("ok", &["a[".to_string(), "ok".to_string()]));
    }

    #[test]
    fn any_pattern_may_match() {
        let patterns = vec!["a/**".to_string(), "**/*.key".to_string()];
        assert!(matches_any("a/x", &patterns));
        assert!(matches_any("b/c.key", &patterns));
        assert!(!matches_any("b/c.txt", &patterns));
    }
}
