//! Go's `path.Match`: the glob syntax core matches *workspace* paths with.
//!
//! Not `filepath.Match`, and not a shell. Three differences decide the port:
//!
//! - The separator is always `/`, on every platform, because workspace paths
//!   are a portable namespace rather than host paths. `*` and `?` never match
//!   a `/`, so a pattern cannot silently span directories.
//! - There are no brace expansions and no `**`. Those live one layer up
//!   (`flowcraft_workspace::glob` splits a pattern on `**` and matches each
//!   segment here), which is why this module is a leaf.
//! - A malformed pattern is an *answer*, not a panic: [`path_match`] returns
//!   [`BadPattern`] -- Go's `ErrBadPattern`, whose text is `syntax error in
//!   pattern` -- and callers decide whether to propagate it. `filepath.Match`
//!   and this function agree on that text because both come from the same
//!   source.
//!
//! The grammar, verbatim from `path.Match`:
//!
//! ```text
//! pattern:
//!     { term }
//! term:
//!     '*'         matches any sequence of non-Separator characters
//!     '?'         matches any single non-Separator character
//!     '[' [ '^' ] { character-range } ']'
//!                 character class (must be non-empty)
//!     c           matches character c (c != '*', '?', '\\', '[')
//!     '\\' c      matches character c
//! character-range:
//!     c           matches character c (c != '\\', '-', ']')
//!     '\\' c      matches character c
//!     lo '-' hi   matches character c for lo <= c <= hi
//! ```
//!
//! Matching is byte-wise where core's is byte-wise -- a literal in a pattern
//! compares one byte -- and rune-wise where core decodes a rune (`?`, a
//! character class). A `[` class that fails to parse is reported even when the
//! name already failed to match, because Go validates the *pattern* on the way
//! out: `match("[a", "z")` is `ErrBadPattern`, not `false`. That property is
//! what makes a bad pattern in a policy file loud instead of silently
//! permissive, so it is preserved exactly.

use std::fmt;

/// The pattern is malformed.
///
/// Go calls this `ErrBadPattern` and prints `syntax error in pattern`; the
/// port keeps the sentence, because it reaches operators through the same
/// wrapping (`Glob`, `matchesAny`) on both sides.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct BadPattern;

impl fmt::Display for BadPattern {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str("syntax error in pattern")
    }
}

impl std::error::Error for BadPattern {}

/// Reports whether `name` matches the shell pattern `pattern`.
///
/// Both are `/`-separated, and `*` / `?` do not cross a `/`. An empty pattern
/// matches only an empty name. A malformed pattern is [`BadPattern`] rather
/// than `false`, including when the name could not have matched anyway.
pub fn path_match(pattern: &str, name: &str) -> Result<bool, BadPattern> {
    let mut pattern = pattern.as_bytes();
    let mut name = name.as_bytes();

    'pattern: while !pattern.is_empty() {
        let (star, chunk, rest) = scan_chunk(pattern);
        pattern = rest;

        if star && chunk.is_empty() {
            // Trailing * matches the rest of the name unless it has a /.
            return Ok(!name.contains(&b'/'));
        }

        // Look for a match at the current position.
        let mut matched = match_chunk(chunk, name)?;
        // A match that leaves name unconsumed only counts when another chunk
        // follows; otherwise the star below may still find one.
        if let Some(tail) = matched {
            if tail.is_empty() || !pattern.is_empty() {
                name = tail;
                continue 'pattern;
            }
            matched = None;
        }
        let _ = matched;

        if star {
            // Try every start position the star could consume, never crossing
            // a separator.
            for i in 0..name.len() {
                if name[i] == b'/' {
                    break;
                }
                if let Some(tail) = match_chunk(chunk, &name[i + 1..])? {
                    // The last chunk must consume the whole name.
                    if pattern.is_empty() && !tail.is_empty() {
                        continue;
                    }
                    name = tail;
                    continue 'pattern;
                }
            }
        }

        // Before answering false, check that what is left of the pattern is
        // syntactically valid: a bad pattern is an error even when nothing
        // could have matched it.
        while !pattern.is_empty() {
            let (_, chunk, rest) = scan_chunk(pattern);
            pattern = rest;
            match_chunk(chunk, b"")?;
        }
        return Ok(false);
    }
    Ok(name.is_empty())
}

/// Splits the leading `*`s and the chunk they decorate off `pattern`.
fn scan_chunk(pattern: &[u8]) -> (bool, &[u8], &[u8]) {
    let mut star = false;
    let mut start = 0;
    while start < pattern.len() && pattern[start] == b'*' {
        start += 1;
        star = true;
    }

    let mut in_range = false;
    let mut i = start;
    while i < pattern.len() {
        match pattern[i] {
            b'\\' => {
                // Whether the escape is well-formed is match_chunk's problem.
                if i + 1 < pattern.len() {
                    i += 1;
                }
            }
            b'[' => in_range = true,
            b']' => in_range = false,
            b'*' if !in_range => break,
            _ => {}
        }
        i += 1;
    }
    (star, &pattern[start..i], &pattern[i..])
}

/// Matches one chunk against the head of `s`.
///
/// `Some(rest)` is the unconsumed tail of `s`. `None` means the chunk did not
/// match *and* the pattern was well-formed: the caller may retry at a later
/// start position. On the way out the chunk is fully scanned even after the
/// match has failed, so a malformed pattern is still reported.
fn match_chunk<'s>(chunk: &[u8], s: &'s [u8]) -> Result<Option<&'s [u8]>, BadPattern> {
    let mut chunk = chunk;
    let mut s = s;
    let mut failed = false;

    while !chunk.is_empty() {
        if !failed && s.is_empty() {
            failed = true;
        }
        match chunk[0] {
            b'[' => {
                // A character class. `r` stays NUL when the match already
                // failed, exactly as Go's zero-valued rune does.
                let mut r = '\0';
                if !failed {
                    let (decoded, n) = decode_rune(s)?;
                    r = decoded;
                    s = &s[n..];
                }
                chunk = &chunk[1..];

                let mut negated = false;
                if !chunk.is_empty() && chunk[0] == b'^' {
                    negated = true;
                    chunk = &chunk[1..];
                }

                let mut hit = false;
                let mut ranges = 0;
                loop {
                    if !chunk.is_empty() && chunk[0] == b']' && ranges > 0 {
                        chunk = &chunk[1..];
                        break;
                    }
                    let (lo, rest) = get_esc(chunk)?;
                    chunk = rest;
                    let mut hi = lo;
                    if !chunk.is_empty() && chunk[0] == b'-' {
                        let (parsed, rest) = get_esc(&chunk[1..])?;
                        hi = parsed;
                        chunk = rest;
                    }
                    if lo <= r && r <= hi {
                        hit = true;
                    }
                    ranges += 1;
                }
                if hit == negated {
                    failed = true;
                }
            }

            b'?' => {
                if !failed {
                    if s[0] == b'/' {
                        failed = true;
                    }
                    let (_, n) = decode_rune(s)?;
                    s = &s[n..];
                }
                chunk = &chunk[1..];
            }

            b'\\' => {
                chunk = &chunk[1..];
                if chunk.is_empty() {
                    return Err(BadPattern);
                }
                if !failed {
                    if chunk[0] != s[0] {
                        failed = true;
                    }
                    s = &s[1..];
                }
                chunk = &chunk[1..];
            }

            c => {
                if !failed {
                    if c != s[0] {
                        failed = true;
                    }
                    s = &s[1..];
                }
                chunk = &chunk[1..];
            }
        }
    }

    if failed {
        return Ok(None);
    }
    Ok(Some(s))
}

/// Parses one character (or escaped character) of a character class.
fn get_esc(chunk: &[u8]) -> Result<(char, &[u8]), BadPattern> {
    if chunk.is_empty() || chunk[0] == b'-' || chunk[0] == b']' {
        return Err(BadPattern);
    }
    let mut chunk = chunk;
    if chunk[0] == b'\\' {
        chunk = &chunk[1..];
        if chunk.is_empty() {
            return Err(BadPattern);
        }
    }
    let (r, n) = decode_rune(chunk)?;
    let rest = &chunk[n..];
    if rest.is_empty() {
        return Err(BadPattern);
    }
    Ok((r, rest))
}

/// Decodes one UTF-8 character, reporting how many bytes it took.
fn decode_rune(s: &[u8]) -> Result<(char, usize), BadPattern> {
    let max = s.len().min(4);
    for take in 1..=max {
        if let Ok(text) = std::str::from_utf8(&s[..take]) {
            // A valid UTF-8 prefix of a valid string is one whole character.
            let Some(head) = text.chars().next() else {
                return Err(BadPattern);
            };
            return Ok((head, take));
        }
    }
    // An invalid sequence, which `&str` input cannot produce but a byte-level
    // port of `utf8.RuneError` must still answer.
    Err(BadPattern)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn literals_and_wildcards() {
        assert_eq!(path_match("", ""), Ok(true));
        assert_eq!(path_match("", "a"), Ok(false));
        assert_eq!(path_match("abc", "abc"), Ok(true));
        assert_eq!(path_match("abc", "abd"), Ok(false));
        assert_eq!(path_match("*", "abc"), Ok(true));
        assert_eq!(path_match("*", ""), Ok(true));
        assert_eq!(path_match("a*", "abc"), Ok(true));
        assert_eq!(path_match("*c", "abc"), Ok(true));
        assert_eq!(path_match("a*c", "abc"), Ok(true));
        assert_eq!(path_match("a*c", "ab"), Ok(false));
        assert_eq!(path_match("?", "a"), Ok(true));
        assert_eq!(path_match("?", ""), Ok(false));
        assert_eq!(path_match("a?c", "abc"), Ok(true));
    }

    #[test]
    fn a_wildcard_never_crosses_a_separator() {
        assert_eq!(path_match("*", "a/b"), Ok(false));
        assert_eq!(path_match("a/*", "a/b"), Ok(true));
        assert_eq!(path_match("a/*", "a/b/c"), Ok(false));
        assert_eq!(path_match("*/*", "a/b"), Ok(true));
        assert_eq!(path_match("a/*/c", "a/b/c"), Ok(true));
        // `?` is one character, and a separator is not one of them.
        assert_eq!(path_match("a?c", "a/c"), Ok(false));
        // The star at the end matches the rest only when there is no separator.
        assert_eq!(path_match("a*", "a/b"), Ok(false));
    }

    #[test]
    fn character_classes() {
        assert_eq!(path_match("[abc]", "b"), Ok(true));
        assert_eq!(path_match("[abc]", "d"), Ok(false));
        assert_eq!(path_match("[a-c]", "b"), Ok(true));
        assert_eq!(path_match("[a-c]", "d"), Ok(false));
        assert_eq!(path_match("[^a-c]", "d"), Ok(true));
        assert_eq!(path_match("[^a-c]", "b"), Ok(false));
        assert_eq!(path_match("x[0-9]", "x7"), Ok(true));
        // A class matches exactly one character, wherever it sits.
        assert_eq!(path_match("*[0-9].go", "a/b7.go"), Ok(false));
        assert_eq!(path_match("*[0-9].go", "ab7.go"), Ok(true));
    }

    #[test]
    fn escapes_are_literal() {
        assert_eq!(path_match(r"a\*b", "a*b"), Ok(true));
        assert_eq!(path_match(r"a\*b", "axb"), Ok(false));
        assert_eq!(path_match(r"\[ab\]", "[ab]"), Ok(true));
        assert_eq!(path_match(r"\\", "\\"), Ok(true));
    }

    #[test]
    fn bad_patterns_are_reported_even_when_nothing_could_match() {
        assert_eq!(path_match("[", "z"), Err(BadPattern));
        assert_eq!(path_match("[a", "z"), Err(BadPattern));
        assert_eq!(path_match("[a-", "z"), Err(BadPattern));
        assert_eq!(path_match("[]", "z"), Err(BadPattern));
        assert_eq!(path_match("[]a]", "z"), Err(BadPattern));
        assert_eq!(path_match("a[", "b"), Err(BadPattern));
        assert_eq!(path_match(r"a\", "b"), Err(BadPattern));
        assert_eq!(path_match("a", "a["), Ok(false));
    }

    #[test]
    fn the_sentence_is_core_s() {
        assert_eq!(BadPattern.to_string(), "syntax error in pattern");
    }

    #[test]
    fn multibyte_names_are_matched_by_rune() {
        assert_eq!(path_match("?", "é"), Ok(true));
        assert_eq!(path_match("[a-z]", "é"), Ok(false));
        assert_eq!(path_match("*", "ü/ü"), Ok(false));
        assert_eq!(path_match("ü", "ü"), Ok(true));
    }
}
