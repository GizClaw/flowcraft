//! Routing keys and their matcher: core's `Subject` and `Pattern`.
//!
//! A subject is a dot-delimited literal (`graph.run.r1.node.n1.complete`); a
//! pattern is the same grammar with `*` for one segment and `>` for one or more
//! trailing ones. Both are parsed here rather than at use time, so a malformed
//! routing key is refused where it is built -- with core's sentence, since a
//! subscriber in another process reads the same key.

use flowcraft_contract::text;

const SEP: char = '.';
const WILDCARD_ONE: &str = "*";
const WILDCARD_TAIL: &str = ">";
const MAX_BYTES: usize = 1024;

/// A literal routing key, e.g. `graph.run.r1.start`.
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct Subject(String);

impl Subject {
    /// core's `Subject.Validate` as a parse.
    pub fn parse(subject: &str) -> Result<Subject, String> {
        let prefix = "event: invalid subject";
        if subject.is_empty() {
            return Err(format!("{prefix}: empty"));
        }
        if subject.len() > MAX_BYTES {
            return Err(format!(
                "{prefix}: {} bytes exceeds max {MAX_BYTES}",
                subject.len()
            ));
        }
        if subject.starts_with(SEP) || subject.ends_with(SEP) {
            return Err(format!(
                "{prefix}: leading or trailing dot in {}",
                text::quote(subject)
            ));
        }
        for (index, segment) in subject.split(SEP).enumerate() {
            if segment.is_empty() {
                return Err(format!(
                    "{prefix}: empty segment at index {index} in {}",
                    text::quote(subject)
                ));
            }
            if segment.contains(['*', '>']) {
                return Err(format!(
                    "{prefix}: wildcard {} present at segment {index} (use Pattern, not Subject)",
                    text::quote(segment)
                ));
            }
        }
        Ok(Subject(subject.to_owned()))
    }

    /// The key itself.
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

/// A subject matcher: `*` matches one segment, `>` one or more trailing ones.
#[derive(Debug, Clone, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct Pattern(String);

impl Pattern {
    /// core's `Pattern.Validate` as a parse.
    pub fn parse(pattern: &str) -> Result<Pattern, String> {
        let prefix = "event: invalid pattern";
        if pattern.is_empty() {
            return Err(format!("{prefix}: empty"));
        }
        if pattern.len() > MAX_BYTES {
            return Err(format!(
                "{prefix}: {} bytes exceeds max {MAX_BYTES}",
                pattern.len()
            ));
        }
        if pattern.starts_with(SEP) || pattern.ends_with(SEP) {
            return Err(format!(
                "{prefix}: leading or trailing dot in {}",
                text::quote(pattern)
            ));
        }
        let segments: Vec<&str> = pattern.split(SEP).collect();
        let last = segments.len() - 1;
        for (index, segment) in segments.iter().enumerate() {
            if segment.is_empty() {
                return Err(format!(
                    "{prefix}: empty segment at index {index} in {}",
                    text::quote(pattern)
                ));
            }
            if *segment == WILDCARD_TAIL {
                if index != last {
                    return Err(format!(
                        "{prefix}: '>' must be the last segment in {}",
                        text::quote(pattern)
                    ));
                }
                continue;
            }
            if *segment == WILDCARD_ONE {
                continue;
            }
            if segment.contains(['*', '>']) {
                return Err(format!(
                    "{prefix}: wildcard must occupy a whole segment, got {}",
                    text::quote(segment)
                ));
            }
        }
        Ok(Pattern(pattern.to_owned()))
    }

    /// The pattern itself.
    pub fn as_str(&self) -> &str {
        &self.0
    }

    /// Whether `subject` satisfies this pattern.
    ///
    /// Well-formed patterns are matched segment by segment; a `>` that is not
    /// the last segment only matches a subject segment that is literally `>`
    /// (core keeps that fallback so the matcher is total for patterns built
    /// without validation; a `Subject` never contains one).
    pub fn matches(&self, subject: &Subject) -> bool {
        match_segments(self.0.split(SEP), subject.0.split(SEP))
    }
}

fn match_segments<'a>(
    pattern: impl Iterator<Item = &'a str> + Clone,
    subject: impl Iterator<Item = &'a str>,
) -> bool {
    let pattern: Vec<&str> = pattern.collect();
    let subject: Vec<&str> = subject.collect();
    let last = pattern.len().saturating_sub(1);
    for (index, segment) in pattern.iter().enumerate() {
        if *segment == WILDCARD_TAIL && index == last {
            return subject.len() > index;
        }
        let Some(candidate) = subject.get(index) else {
            return false;
        };
        if *segment == WILDCARD_ONE {
            continue;
        }
        if segment != candidate {
            return false;
        }
    }
    pattern.len() == subject.len()
}

/// The leading segment of `subject` -- by SDK convention the producer kind
/// (`engine`, `kanban`, ...). Empty when the subject is empty or single-segment.
pub fn producer_kind(subject: &Subject) -> &str {
    match subject.as_str().split_once(SEP) {
        Some((kind, _)) if !kind.is_empty() => kind,
        _ => "",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The sentence a rule the test expects to be refused answered with.
    fn refused(result: Result<impl std::fmt::Debug, String>, what: &str) -> String {
        match result {
            Ok(value) => panic!("{what}: expected a refusal, got {value:?}"),
            Err(err) => err,
        }
    }

    fn subject(value: &str) -> Subject {
        match Subject::parse(value) {
            Ok(subject) => subject,
            Err(err) => panic!("{value:?} should be a subject: {err}"),
        }
    }

    fn pattern(value: &str) -> Pattern {
        match Pattern::parse(value) {
            Ok(pattern) => pattern,
            Err(err) => panic!("{value:?} should be a pattern: {err}"),
        }
    }

    #[test]
    fn a_subject_is_segments_without_wildcards() {
        assert_eq!(subject("graph.run.r1.start").as_str(), "graph.run.r1.start");
        for bad in ["", ".a", "a.", "a..b", "a.*", "a.>", "kanban.*.b1"] {
            assert!(Subject::parse(bad).is_err(), "{bad:?} should be refused");
        }
        assert!(Subject::parse(&"a".repeat(1024)).is_ok());
        assert_eq!(
            refused(
                Subject::parse(&"a".repeat(1025)),
                "a subject over the limit"
            ),
            "event: invalid subject: 1025 bytes exceeds max 1024"
        );
    }

    #[test]
    fn a_pattern_matches_segment_by_segment() {
        assert!(pattern("graph.run.r1.>").matches(&subject("graph.run.r1.node.n1.start")));
        assert!(pattern("graph.run.>").matches(&subject("graph.run.r1")));
        assert!(!pattern("graph.run.>").matches(&subject("graph.run")));
        assert!(
            pattern("graph.run.*.node.*.start").matches(&subject("graph.run.r1.node.n1.start"))
        );
        assert!(!pattern("graph.run.*").matches(&subject("graph.run.r1.start")));
        assert!(pattern("kanban.>").matches(&subject("kanban.board.b1.update")));
        assert!(pattern("a.b").matches(&subject("a.b")));
        assert!(!pattern("a.b").matches(&subject("a.b.c")));
    }

    #[test]
    fn a_pattern_refuses_a_tail_wildcard_anywhere_but_last() {
        assert_eq!(
            refused(Pattern::parse("a.>.b"), "a tail wildcard in the middle"),
            "event: invalid pattern: '>' must be the last segment in \"a.>.b\""
        );
        assert_eq!(
            refused(Pattern::parse("a.b*"), "a wildcard inside a segment"),
            "event: invalid pattern: wildcard must occupy a whole segment, got \"b*\""
        );
        assert!(Pattern::parse("a.*.b").is_ok());
    }

    #[test]
    fn the_producer_is_the_leading_segment() {
        assert_eq!(producer_kind(&subject("engine.run.start")), "engine");
        assert_eq!(producer_kind(&subject("kanban")), "");
        assert_eq!(producer_kind(&subject("a.b")), "a");
    }
}
