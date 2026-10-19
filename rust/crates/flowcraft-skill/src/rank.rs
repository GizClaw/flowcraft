//! `core/skill/rank.go`: explicit `$name` mentions and BM25 ranked search.
//!
//! Two ways a skill reaches a turn: the user names it (`$review`), or the turn's
//! text scores against the registry. Both answer with [`Metadata`] in the order
//! they were resolved, and both deduplicate by path -- the same skill reachable
//! twice is one entry.
//!
//! # What the port changed
//!
//! - Mentions are matched by [`mention_names`], a scanner, rather than by a
//!   `regexp`. The grammar is one line (ASCII, one optional leading non-word
//!   character, a `$`, a lowercase slug), and the scanner reproduces the one
//!   property that matters: the character before `$` is *consumed* by a match,
//!   so `$a$b` names one skill, exactly as Go's non-overlapping
//!   `FindAllStringSubmatch` does. `rust/conformance/skill-registry-contract.json`
//!   has the rows that pin it.
//! - A limit of `0` means "use [`Service::top_n`](crate::Service::top_n)"; core
//!   spells the same rule as `<= 0` on a signed integer, so the negative half
//!   is unreachable here rather than untested.
//! - An empty result is an empty [`Vec`] rather than a `nil` slice. Every caller
//!   in core treats them the same way ("nothing to inject").

use crate::parse::Metadata;
use crate::service::Service;

/// One ranked skill and its BM25 score, for observability (search output,
/// telemetry, threshold tuning).
#[derive(Debug, Clone, PartialEq)]
pub struct Scored {
    /// The skill the score belongs to.
    pub skill: Metadata,
    /// Its BM25 score.
    pub score: f64,
}

impl Service {
    /// Resolves explicit `$name` mentions to skills in mention order, keeping
    /// the first mention of each skill.
    pub fn mentioned(&self, text: &str) -> Vec<Metadata> {
        let mut out: Vec<Metadata> = Vec::new();
        for name in mention_names(text) {
            let Some(skill) = self.by_name(&name) else {
                continue;
            };
            // The same skill reachable through two identical mentions is one
            // entry; a different skill that happens to share the name is
            // shadowed by the first, as `by_name` defines.
            if out.iter().any(|seen| seen.path == skill.path) {
                continue;
            }
            out.push(skill);
        }
        out
    }

    /// The top `top_n` skills for `query` that score at least `min_score`,
    /// sorted by score descending.
    ///
    /// `min_score <= 0` accepts any match, and `top_n == 0` falls back to
    /// [`Service::top_n`]. An empty query -- or one that matches nothing --
    /// returns an empty list, so an empty result reads as "nothing to inject".
    pub fn rank(&self, query: &str, top_n: usize, min_score: f64) -> Vec<Metadata> {
        self.rank_scored(query, top_n, min_score)
            .into_iter()
            .map(|scored| scored.skill)
            .collect()
    }

    /// [`rank`](Service::rank) with scores attached.
    pub fn rank_scored(&self, query: &str, top_n: usize, min_score: f64) -> Vec<Scored> {
        let snapshot = self.snapshot();
        let Some(index) = snapshot.index.as_ref() else {
            return Vec::new();
        };
        if query.trim().is_empty() {
            return Vec::new();
        }
        let limit = if top_n == 0 { self.top_n() } else { top_n };
        let mut out = Vec::new();
        for hit in index.search(query, Some(limit)) {
            if min_score > 0.0 && hit.score < min_score {
                continue;
            }
            if let Some(skill) = snapshot.by_path.get(&hit.id) {
                out.push(Scored {
                    skill: skill.clone(),
                    score: hit.score,
                });
            }
        }
        out
    }
}

/// Extracts `$name` mentions from text, in the order they appear.
///
/// The grammar is core's `(?:^|[^a-z0-9_])\$([a-z0-9]+(?:-[a-z0-9]+)*)`, and so
/// is its scanning rule: a match consumes the character before the `$`, which
/// is why `$50` inside `$500` is not a mention (`0` is a word character), why
/// `a$skill` is not one either, and why `$a$b` yields only `a`.
pub fn mention_names(text: &str) -> Vec<String> {
    let bytes = text.as_bytes();
    let mut out = Vec::new();
    let mut at = 0;
    while at < bytes.len() {
        // The `^` alternative is zero-width; the other one is exactly one
        // non-word character, which a match then consumes.
        let mut cursor = if at == 0 {
            at
        } else if is_word(bytes[at]) {
            at += 1;
            continue;
        } else {
            at + 1
        };
        if cursor < bytes.len() && bytes[cursor] == b'$' {
            cursor += 1;
            let start = cursor;
            while cursor < bytes.len() && is_lower_or_digit(bytes[cursor]) {
                cursor += 1;
            }
            if cursor > start {
                // `(?:-[a-z0-9]+)*`: each segment needs at least one character,
                // so a trailing `-` is not part of the name.
                loop {
                    if cursor + 1 >= bytes.len()
                        || bytes[cursor] != b'-'
                        || !is_lower_or_digit(bytes[cursor + 1])
                    {
                        break;
                    }
                    cursor += 1;
                    while cursor < bytes.len() && is_lower_or_digit(bytes[cursor]) {
                        cursor += 1;
                    }
                }
                out.push(text[start..cursor].to_owned());
                at = cursor;
                continue;
            }
        }
        at += 1;
    }
    out
}

/// `[a-z0-9_]`.
fn is_word(byte: u8) -> bool {
    is_lower_or_digit(byte) || byte == b'_'
}

/// `[a-z0-9]`.
fn is_lower_or_digit(byte: u8) -> bool {
    byte.is_ascii_lowercase() || byte.is_ascii_digit()
}
