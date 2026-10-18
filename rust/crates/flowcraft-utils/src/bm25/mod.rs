//! `core/utils/bm25`: a small in-memory BM25 index over a fixed document set.
//!
//! A catalog of a few hundred to a few thousand short documents -- tool
//! definitions, skill files, retrieved records -- is exactly the regime where a
//! hand-rolled inverted index beats pulling in a search engine.
//!
//! A document carries a `name` (an identifier such as a tool or command name)
//! and a `text` (its description). Name hits score higher than text hits, and a
//! query term also matches index terms by prefix, so a fragment like `webf`
//! still surfaces `webfetch`. A query term contributes its *best* match in each
//! field rather than the sum of all of them, so a document that merely prefixes
//! several index terms cannot outrank one that contains the term itself.
//!
//! The index keeps term statistics only -- it does not retain document text --
//! and is immutable once built, so searches can run on several threads.
//!
//! # What the port changed
//!
//! The scoring arithmetic is the same expression, in the same order, because a
//! ranking difference between the two sides would be a silent behaviour change
//! (the contract dump compares scores and order). The *shape* is Rust's: the
//! two fields are a `[FieldIndex; 2]` like core's array, the postings are still
//! one flat term-major slice, and the `i32` document numbers and frequencies
//! stay `i32` -- the regime is a few thousand short documents, and a corpus
//! outside it is refused where core would silently wrap.
//!
//! `Search(query, limit)` takes `Option<usize>` where core takes an `int` that
//! means "no limit" at zero or below: an absent limit is a different thing from
//! a limit of zero, and the type says so.

mod tokenize;

use std::collections::BTreeMap;

use flowcraft_contract::text;

pub use tokenize::{is_cjk, tokenize};

/// The standard Okapi BM25 term-frequency saturation.
pub const DEFAULT_K1: f64 = 1.2;

/// The standard Okapi BM25 length-normalisation strength.
pub const DEFAULT_B: f64 = 0.75;

/// The discount a query term gets when it only prefixes an index term.
pub const DEFAULT_PREFIX_WEIGHT: f64 = 0.5;

/// A term in the name field scores this much higher than the same term in the
/// text field.
const NAME_BOOST: f64 = 3.0;
/// The text field's weight, named so the pair reads as the pair it is.
const TEXT_BOOST: f64 = 1.0;
const FIELD_BOOSTS: [f64; 2] = [NAME_BOOST, TEXT_BOOST];

const NAME_FIELD: usize = 0;
const TEXT_FIELD: usize = 1;

/// One indexed document.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Doc {
    /// Identifies the document in results.
    pub id: String,
    /// The boosted field: identifiers such as tool or command names.
    pub name: String,
    /// The body field: descriptions.
    pub text: String,
}

impl Doc {
    /// A document from anything string-like.
    pub fn new(id: impl Into<String>, name: impl Into<String>, text: impl Into<String>) -> Doc {
        Doc {
            id: id.into(),
            name: name.into(),
            text: text.into(),
        }
    }
}

/// One indexed document whose fields are already tokenized.
///
/// For a caller that persists or caches tokens -- a projection storing
/// per-document term counts, say -- rather than re-tokenizing text.
///
/// The terms must come from [`tokenize`] (or a splitter that splits the same
/// way): queries are always tokenized, so terms produced by a different
/// tokenizer may never match. Frequencies must be positive.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TermDoc {
    /// Identifies the document in results.
    pub id: String,
    /// Term frequencies of the name field.
    pub name: BTreeMap<String, i32>,
    /// Term frequencies of the text field.
    pub text: BTreeMap<String, i32>,
}

/// One match, ranked by descending score.
///
/// core calls this `Result`; the port spells it `Match` because `Result` is
/// too load-bearing a name in Rust to shadow -- every signature below would
/// have had to say `std::result::Result`. Nothing else about it moved.
#[derive(Debug, Clone, PartialEq)]
pub struct Match {
    /// The document's id.
    pub id: String,
    /// Its BM25 score.
    pub score: f64,
}

/// The scoring configuration, core's `Option` set resolved.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Options {
    /// Term-frequency saturation; finite and positive.
    pub k1: f64,
    /// Length-normalisation strength, in `[0, 1]`.
    pub b: f64,
    /// Discount for a prefix-only match, in `[0, 1]`; zero disables prefix
    /// matching entirely.
    pub prefix_weight: f64,
}

impl Default for Options {
    fn default() -> Options {
        Options {
            k1: DEFAULT_K1,
            b: DEFAULT_B,
            prefix_weight: DEFAULT_PREFIX_WEIGHT,
        }
    }
}

impl Options {
    /// core's `WithK1`.
    pub fn with_k1(mut self, k1: f64) -> Options {
        self.k1 = k1;
        self
    }

    /// core's `WithB`. Zero disables length normalisation rather than falling
    /// back to the default.
    pub fn with_b(mut self, b: f64) -> Options {
        self.b = b;
        self
    }

    /// core's `WithPrefixWeight`.
    pub fn with_prefix_weight(mut self, weight: f64) -> Options {
        self.prefix_weight = weight;
        self
    }

    /// core's `config.validate`, sentence for sentence.
    pub fn validate(self) -> Result<Options, Error> {
        if !self.k1.is_finite() || self.k1 <= 0.0 {
            return Err(Error::InvalidOption("bm25: k1 must be finite and positive"));
        }
        if !self.b.is_finite() || !(0.0..=1.0).contains(&self.b) {
            return Err(Error::InvalidOption("bm25: b must be in [0,1]"));
        }
        if !self.prefix_weight.is_finite() || !(0.0..=1.0).contains(&self.prefix_weight) {
            return Err(Error::InvalidOption("bm25: prefix weight must be in [0,1]"));
        }
        Ok(self)
    }
}

/// What the index refuses.
///
/// The sentences are core's, `%q` and all, because they reach a caller through
/// a tool error and the two sides should read the same.
#[derive(Debug, Clone, PartialEq, Eq)]
#[non_exhaustive]
pub enum Error {
    /// A scoring option is out of range; the message is the sentence.
    InvalidOption(&'static str),
    /// Two documents share an id, so a result could not be mapped back to one.
    DuplicateId(String),
    /// A pre-tokenized field carries an empty term.
    EmptyTerm {
        /// The document the term belongs to.
        id: String,
        /// `name` or `text`.
        field: &'static str,
    },
    /// A pre-tokenized field carries a frequency that is not positive, or one
    /// that does not fit the index's `i32`.
    InvalidFrequency {
        /// The document the term belongs to.
        id: String,
        /// `name` or `text`.
        field: &'static str,
        /// The term.
        term: String,
        /// What the caller asked for.
        frequency: i64,
    },
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Error::InvalidOption(message) => f.write_str(message),
            Error::DuplicateId(id) => {
                write!(f, "bm25: duplicate document id {}", text::quote(id))
            }
            Error::EmptyTerm { id, field } => {
                write!(f, "bm25: document {}: empty {field} term", text::quote(id))
            }
            Error::InvalidFrequency {
                id,
                field,
                term,
                frequency,
            } => write!(
                f,
                "bm25: document {}: {field} term {}: invalid frequency {frequency}",
                text::quote(id),
                text::quote(term)
            ),
        }
    }
}

impl std::error::Error for Error {}

/// One distinct term and the slice of postings it owns.
#[derive(Debug, Clone)]
struct TermEntry {
    term: String,
    off: i32,
    df: i32,
}

/// One document's term frequency for the owning term.
#[derive(Debug, Clone, Copy)]
struct Posting {
    doc: i32,
    tf: i32,
}

/// A collected posting before the terms are folded together.
struct PendingPosting {
    term: String,
    doc: i32,
    tf: i32,
}

/// The per-field statistics a BM25 score needs: each document's field length
/// and the postings of every term.
///
/// The postings are a flat, term-major slice instead of a `map[term][]posting`
/// so a lookup is one binary search and the whole structure is one allocation
/// after finalisation.
#[derive(Debug, Default)]
struct FieldIndex {
    lengths: Vec<i64>,
    total_len: i64,
    terms: Vec<TermEntry>,
    postings: Vec<Posting>,
    /// Documents divided by length; zero when there are no documents.
    avg: f64,
}

/// One index term a query term resolves to.
struct TermMatch {
    term: String,
    df: i32,
    /// True when the query term is only a prefix of this index term.
    prefix: bool,
}

impl FieldIndex {
    /// Fold the collected postings into the flat tables: sorted by term, then
    /// by document, so every term's postings stay in document order (which is
    /// what lets [`Index::doc_freq`] merge two fields without a set).
    fn finalize(mut pending: Vec<PendingPosting>, lengths: Vec<i64>, total_len: i64) -> FieldIndex {
        pending.sort_unstable_by(|left, right| {
            left.term
                .cmp(&right.term)
                .then_with(|| left.doc.cmp(&right.doc))
        });
        let mut field = FieldIndex {
            lengths,
            total_len,
            postings: Vec::with_capacity(pending.len()),
            terms: Vec::new(),
            avg: 0.0,
        };
        for posting in pending {
            match field.terms.last_mut() {
                Some(entry) if entry.term == posting.term => entry.df += 1,
                _ => field.terms.push(TermEntry {
                    term: posting.term,
                    off: field.postings.len() as i32,
                    df: 1,
                }),
            }
            field.postings.push(Posting {
                doc: posting.doc,
                tf: posting.tf,
            });
        }
        field
    }

    /// The position of the first term >= target, and whether it is exact.
    fn search(&self, target: &str) -> (usize, bool) {
        let mut low = 0;
        let mut high = self.terms.len();
        while low < high {
            let middle = (low + high) / 2;
            if self.terms[middle].term.as_str() < target {
                low = middle + 1;
            } else {
                high = middle;
            }
        }
        (
            low,
            low < self.terms.len() && self.terms[low].term == target,
        )
    }

    fn lookup(&self, term: &str) -> Option<&TermEntry> {
        match self.search(term) {
            (_, false) => None,
            (index, true) => Some(&self.terms[index]),
        }
    }

    fn postings_of(&self, term: &str) -> &[Posting] {
        match self.lookup(term) {
            Some(entry) => {
                let start = entry.off as usize;
                &self.postings[start..start + entry.df as usize]
            }
            None => &[],
        }
    }

    /// Call `each` for every indexed term that has `prefix` as a *proper*
    /// prefix. Terms are sorted, so the scan starts at the first term >= prefix
    /// and stops at the first one that no longer matches.
    fn each_prefix(&self, prefix: &str, mut each: impl FnMut(&TermEntry)) {
        let (start, _) = self.search(prefix);
        for entry in &self.terms[start..] {
            if !entry.term.starts_with(prefix) {
                return;
            }
            if entry.term.len() > prefix.len() {
                each(entry);
            }
        }
    }
}

/// An immutable BM25 index over a fixed document set.
///
/// An index built here answers every query with the same scores on every run
/// and can be searched from several threads; there is nothing to lock.
#[derive(Debug)]
pub struct Index {
    ids: Vec<String>,
    fields: [FieldIndex; 2],
    n_docs: usize,
    options: Options,
}

impl Index {
    /// Build an index over `docs`, tokenizing `name` and `text`.
    ///
    /// Document ids must be unique: a repeated id is refused, because results
    /// are keyed by id and a caller mapping a result back to its document would
    /// silently pick one of the duplicates. Ties in [`search`](Index::search)
    /// keep the order the documents arrive in.
    pub fn new(docs: &[Doc], options: Options) -> Result<Index, Error> {
        let options = options.validate()?;
        let mut ids = Vec::with_capacity(docs.len());
        let mut name_pending = Vec::new();
        let mut text_pending = Vec::new();
        let mut name_field = FieldIndex::default();
        let mut text_field = FieldIndex::default();
        let mut seen: std::collections::HashSet<&str> =
            std::collections::HashSet::with_capacity(docs.len());

        for (number, doc) in docs.iter().enumerate() {
            if !seen.insert(doc.id.as_str()) {
                return Err(Error::DuplicateId(doc.id.clone()));
            }
            ids.push(doc.id.clone());
            add_text(&mut name_field, &mut name_pending, number as i32, &doc.name);
            add_text(&mut text_field, &mut text_pending, number as i32, &doc.text);
        }

        let mut index = Index {
            ids,
            fields: [
                FieldIndex::finalize(name_pending, name_field.lengths, name_field.total_len),
                FieldIndex::finalize(text_pending, text_field.lengths, text_field.total_len),
            ],
            n_docs: docs.len(),
            options,
        };
        index.finish();
        Ok(index)
    }

    /// Build an index over documents whose fields are already tokenized.
    ///
    /// Terms come from [`tokenize`] (or an equivalent splitter); frequencies
    /// must be positive, and a term that repeats nowhere still contributes its
    /// length.
    pub fn from_terms(docs: &[TermDoc], options: Options) -> Result<Index, Error> {
        let options = options.validate()?;
        let mut ids = Vec::with_capacity(docs.len());
        let mut name_field = FieldIndex::default();
        let mut text_field = FieldIndex::default();
        let mut name_pending = Vec::new();
        let mut text_pending = Vec::new();
        let mut seen: std::collections::HashSet<&str> =
            std::collections::HashSet::with_capacity(docs.len());

        for (number, doc) in docs.iter().enumerate() {
            if !seen.insert(doc.id.as_str()) {
                return Err(Error::DuplicateId(doc.id.clone()));
            }
            ids.push(doc.id.clone());
            add_terms(
                &mut name_field,
                &mut name_pending,
                number as i32,
                &doc.id,
                "name",
                &doc.name,
            )?;
            add_terms(
                &mut text_field,
                &mut text_pending,
                number as i32,
                &doc.id,
                "text",
                &doc.text,
            )?;
        }

        let mut index = Index {
            ids,
            fields: [
                FieldIndex::finalize(name_pending, name_field.lengths, name_field.total_len),
                FieldIndex::finalize(text_pending, text_field.lengths, text_field.total_len),
            ],
            n_docs: docs.len(),
            options,
        };
        index.finish();
        Ok(index)
    }

    /// The number of indexed documents.
    pub fn len(&self) -> usize {
        self.n_docs
    }

    /// Whether the index is empty.
    pub fn is_empty(&self) -> bool {
        self.n_docs == 0
    }

    /// The configuration the index was built with.
    pub fn options(&self) -> Options {
        self.options
    }

    fn finish(&mut self) {
        for field in &mut self.fields {
            if self.n_docs > 0 {
                field.avg = field.total_len as f64 / self.n_docs as f64;
            }
        }
    }

    /// Rank documents against `query`.
    ///
    /// Query terms also match by prefix when prefix weighting is enabled; a
    /// query term contributes the best of its exact and prefix matches, so
    /// prefix noise never sums past an exact hit. Ties keep the order the
    /// documents were passed to [`new`](Index::new). A query with no indexable
    /// terms and an empty index both answer with an empty list; `limit` of
    /// `None` means no limit (core's `limit <= 0`).
    pub fn search(&self, query: &str, limit: Option<usize>) -> Vec<Match> {
        self.search_terms(&tokenize(query), limit)
    }

    /// [`search`](Index::search) over an already-tokenized query.
    ///
    /// The terms must be split the way the indexed fields were, or queries
    /// cannot match. Repeated terms count once, as in `search`.
    pub fn search_terms(&self, query: &[String], limit: Option<usize>) -> Vec<Match> {
        let terms = unique_terms(query);
        if terms.is_empty() || self.n_docs == 0 {
            return Vec::new();
        }

        let mut scores = vec![0.0f64; self.n_docs];
        let mut best = vec![0.0f64; self.n_docs];
        let mut touched: Vec<usize> = Vec::with_capacity(16);
        for group in self.term_matches(&terms) {
            touched = self.add_to_scores(&mut scores, &mut best, touched, &group);
        }

        let mut ranked: Vec<Match> = scores
            .iter()
            .enumerate()
            .filter(|(_, score)| **score > 0.0)
            .map(|(document, score)| Match {
                id: self.ids[document].clone(),
                score: *score,
            })
            .collect();
        if ranked.is_empty() {
            return ranked;
        }
        // Stable, so equal scores keep the caller's document order.
        ranked.sort_by(|left, right| {
            right
                .score
                .partial_cmp(&left.score)
                .unwrap_or(std::cmp::Ordering::Equal)
        });
        if let Some(limit) = limit {
            ranked.truncate(limit);
        }
        ranked
    }

    /// Expand the unique query terms to groups of index terms: each term
    /// itself plus, when prefix weighting is enabled, every index term that has
    /// it as a proper prefix.
    ///
    /// An index term that several query terms resolve to is scored once, in the
    /// group of the first query term that reached it, at the exact weight if
    /// any query term equals it -- so a term that is both a query term and
    /// another term's prefix hit is not scored twice.
    fn term_matches(&self, terms: &[String]) -> Vec<Vec<TermMatch>> {
        let mut groups: Vec<Vec<TermMatch>> = Vec::with_capacity(terms.len());
        let mut claimed: BTreeMap<String, (usize, usize)> = BTreeMap::new();

        for term in terms {
            let mut group: Vec<TermMatch> = Vec::with_capacity(2);
            let mut additions: Vec<String> = Vec::new();
            for field in &self.fields {
                field.each_prefix(term, |entry| {
                    additions.push(entry.term.clone());
                });
            }
            // The exact term first: `claimed` gives the first placement, and an
            // exact match later overwrites a prefix's discount.
            let exact = if self.fields.iter().any(|field| field.lookup(term).is_some()) {
                Some(term.clone())
            } else {
                None
            };
            let candidates = exact.into_iter().chain(additions);
            for candidate in candidates {
                let prefix = candidate != *term;
                match claimed.get(&candidate) {
                    Some((at_group, at)) => {
                        if !prefix {
                            groups[*at_group][*at].prefix = false;
                        }
                    }
                    None => {
                        // The document frequency is the union across fields;
                        // taking one field's would double-count documents that
                        // mention the term in both.
                        let df = self.doc_freq(&candidate);
                        claimed.insert(candidate.clone(), (groups.len(), group.len()));
                        group.push(TermMatch {
                            term: candidate,
                            df,
                            prefix,
                        });
                    }
                }
            }
            groups.push(group);
        }
        groups
    }

    /// The Okapi inverse document frequency with the usual smoothing: a term
    /// that appears in every document keeps a small positive weight instead of
    /// collapsing to zero.
    fn idf(&self, df: i32) -> f64 {
        let documents = self.n_docs as f64;
        let frequency = f64::from(df);
        // `ln(1 + x)`, not `ln_1p(x)`: the contract compares numbers, and the
        // two spellings differ in the last bit.
        (1.0 + (documents - frequency + 0.5) / (frequency + 0.5)).ln()
    }

    /// Fold one query term's contribution into `scores`: per field, the group's
    /// best match for a document, never the sum over the group.
    ///
    /// A document that prefixes four expansions of one query term would
    /// otherwise outrank a document that contains that term. `best` holds the
    /// running maximum while a group is scored, and `touched` lists the
    /// documents it was written for, so resetting costs the postings a group
    /// actually has rather than the size of the corpus. `touched` is reused
    /// across groups and handed back for the next one.
    fn add_to_scores(
        &self,
        scores: &mut [f64],
        best: &mut [f64],
        mut touched: Vec<usize>,
        group: &[TermMatch],
    ) -> Vec<usize> {
        let options = self.options;
        for (field_number, boost) in FIELD_BOOSTS.iter().enumerate() {
            let field = &self.fields[field_number];
            if field.avg == 0.0 {
                continue;
            }
            for candidate in group {
                let weight = if candidate.prefix {
                    boost * options.prefix_weight
                } else {
                    *boost
                };
                let idf = self.idf(candidate.df);
                for posting in field.postings_of(&candidate.term) {
                    let tf = f64::from(posting.tf);
                    let length = field.lengths[posting.doc as usize] as f64;
                    let denominator =
                        tf + options.k1 * (1.0 - options.b + options.b * length / field.avg);
                    let score = weight * idf * tf * (options.k1 + 1.0) / denominator;
                    let document = posting.doc as usize;
                    if score > best[document] {
                        if best[document] == 0.0 {
                            touched.push(document);
                        }
                        best[document] = score;
                    }
                }
            }
            for document in &touched {
                scores[*document] += best[*document];
                best[*document] = 0.0;
            }
            touched.clear();
        }
        touched
    }

    /// How many documents contain `term` in either field.
    ///
    /// A document that hits in both its name and its text counts once: this is
    /// the union across fields, not the sum.
    fn doc_freq(&self, term: &str) -> i32 {
        let name = self.fields[NAME_FIELD].postings_of(term);
        let text = self.fields[TEXT_FIELD].postings_of(term);
        if name.is_empty() || text.is_empty() {
            return (name.len() + text.len()) as i32;
        }
        // Both lists are in document order: walk them together.
        let (mut count, mut left, mut right) = (0i32, 0usize, 0usize);
        while left < name.len() && right < text.len() {
            match name[left].doc.cmp(&text[right].doc) {
                std::cmp::Ordering::Equal => {
                    left += 1;
                    right += 1;
                }
                std::cmp::Ordering::Less => left += 1,
                std::cmp::Ordering::Greater => right += 1,
            }
            count += 1;
        }
        count + (name.len() - left) as i32 + (text.len() - right) as i32
    }
}

/// Tokenize and add one document's text for a field.
///
/// Terms are deduplicated by sorting the token slice in place, so adding a
/// document allocates no per-document map. The field length is the total token
/// count, repetitions included -- the standard BM25 `|D|`: term frequencies
/// saturate in the score, so a repeated term adds length without exploding its
/// contribution.
fn add_text(field: &mut FieldIndex, pending: &mut Vec<PendingPosting>, document: i32, text: &str) {
    let mut tokens = tokenize(text);
    field.lengths.push(tokens.len() as i64);
    field.total_len += tokens.len() as i64;
    tokens.sort_unstable();
    let mut start = 0;
    while start < tokens.len() {
        let mut end = start + 1;
        while end < tokens.len() && tokens[end] == tokens[start] {
            end += 1;
        }
        pending.push(PendingPosting {
            term: tokens[start].clone(),
            doc: document,
            tf: (end - start) as i32,
        });
        start = end;
    }
}

/// Add one document's pre-tokenized field.
///
/// Frequencies must be positive, so a caller cannot smuggle in a document that
/// contributes nothing but length.
fn add_terms(
    field: &mut FieldIndex,
    pending: &mut Vec<PendingPosting>,
    document: i32,
    id: &str,
    field_name: &'static str,
    terms: &BTreeMap<String, i32>,
) -> Result<(), Error> {
    let mut total: i64 = 0;
    for (term, frequency) in terms {
        if term.is_empty() {
            return Err(Error::EmptyTerm {
                id: id.to_owned(),
                field: field_name,
            });
        }
        if *frequency <= 0 {
            return Err(Error::InvalidFrequency {
                id: id.to_owned(),
                field: field_name,
                term: term.clone(),
                frequency: i64::from(*frequency),
            });
        }
        total += i64::from(*frequency);
        pending.push(PendingPosting {
            term: term.clone(),
            doc: document,
            tf: *frequency,
        });
    }
    field.lengths.push(total);
    field.total_len += total;
    Ok(())
}

/// Keep each distinct non-empty term once, in first-appearance order.
///
/// A repeated word cannot double-count its IDF contribution.
fn unique_terms(terms: &[String]) -> Vec<String> {
    let mut seen: std::collections::HashSet<&str> =
        std::collections::HashSet::with_capacity(terms.len());
    let mut unique: Vec<String> = Vec::with_capacity(terms.len());
    for term in terms {
        if term.is_empty() {
            continue;
        }
        if !seen.insert(term.as_str()) {
            continue;
        }
        unique.push(term.clone());
    }
    unique
}

#[cfg(test)]
mod tests {
    use super::*;

    fn docs() -> Vec<Doc> {
        vec![
            Doc::new("fetch", "webfetch", "fetch a URL and return its text"),
            Doc::new("search", "websearch", "search the web for pages"),
            Doc::new("read", "fileread", "read a file from the workspace"),
            Doc::new("bm", "bookmark", "resume reading position in a **resume**"),
        ]
    }

    fn index(options: Options) -> Index {
        match Index::new(&docs(), options) {
            Ok(index) => index,
            Err(err) => panic!("index: {err}"),
        }
    }

    fn ids(results: &[Match]) -> Vec<&str> {
        results.iter().map(|result| result.id.as_str()).collect()
    }

    #[test]
    fn name_hits_outrank_text_hits() {
        let index = index(Options::default());
        // "web" is a prefix of both names; "fetch" is a whole name term of one
        // and a text term of another.
        let found = index.search("webfetch", None);
        let ranked = ids(&found);
        assert_eq!(ranked.first(), Some(&"fetch"));
    }

    #[test]
    fn an_exact_term_outranks_a_prefix_of_it() {
        let index = index(Options::default());
        let found = index.search("resume", None);
        let ranked = &found;
        // "resume" appears as a text term in both docs that have it; what
        // matters is that the doc with it twice still ranks first.
        assert_eq!(ids(ranked).first(), Some(&"bm"));
    }

    #[test]
    fn prefix_matching_can_be_switched_off() {
        let index = index(Options::default().with_prefix_weight(0.0));
        let found = index.search("webf", None);
        let ranked = ids(&found);
        assert!(
            ranked.is_empty(),
            "with no prefix weight, a fragment matches nothing: {ranked:?}"
        );
    }

    #[test]
    fn a_repeated_query_term_counts_once() {
        let index = index(Options::default());
        let once = index.search("fetch", None);
        let twice = index.search("fetch fetch", None);
        assert_eq!(once, twice);
    }

    #[test]
    fn cjk_queries_match_fragments() {
        let docs = vec![
            Doc::new("read", "读取工具", "从工作区读取文件"),
            Doc::new("write", "写入工具", "向工作区写入文件"),
        ];
        let index = match Index::new(&docs, Options::default()) {
            Ok(index) => index,
            Err(err) => panic!("index: {err}"),
        };
        let both = index.search("文件", None);
        assert_eq!(ids(&both).len(), 2, "both mention 文件: {:?}", ids(&both));
        let read = index.search("读取", None);
        assert_eq!(ids(&read).first(), Some(&"read"));
    }

    #[test]
    fn ties_keep_document_order() {
        let docs = vec![
            Doc::new("b", "same", "same"),
            Doc::new("a", "same", "same"),
            Doc::new("c", "same", "same"),
        ];
        let index = match Index::new(&docs, Options::default()) {
            Ok(index) => index,
            Err(err) => panic!("index: {err}"),
        };
        assert_eq!(ids(&index.search("same", None)), vec!["b", "a", "c"]);
    }

    #[test]
    fn the_limit_truncates_after_ranking() {
        let index = index(Options::default());
        let ranked = index.search("web", Some(1));
        assert_eq!(ranked.len(), 1);
        assert_eq!(index.search("web", None).first(), ranked.first());
    }

    #[test]
    fn a_duplicate_id_is_refused() {
        let docs = vec![Doc::new("x", "a", ""), Doc::new("x", "b", "")];
        let error = match Index::new(&docs, Options::default()) {
            Ok(_) => panic!("two documents with one id"),
            Err(err) => err,
        };
        assert_eq!(error, Error::DuplicateId("x".to_owned()));
        assert_eq!(error.to_string(), "bm25: duplicate document id \"x\"");
    }

    #[test]
    fn invalid_options_are_refused_in_cores_order() {
        for (options, expected) in [
            (
                Options::default().with_k1(0.0),
                "bm25: k1 must be finite and positive",
            ),
            (
                Options::default().with_k1(f64::NAN),
                "bm25: k1 must be finite and positive",
            ),
            (Options::default().with_b(1.5), "bm25: b must be in [0,1]"),
            (
                Options::default().with_prefix_weight(-0.1),
                "bm25: prefix weight must be in [0,1]",
            ),
        ] {
            let docs = vec![Doc::new("x", "a", "b")];
            match Index::new(&docs, options) {
                Ok(_) => panic!("{expected}"),
                Err(err) => assert_eq!(err.to_string(), expected),
            }
        }
    }

    #[test]
    fn pre_tokenized_fields_are_checked() {
        let mut name = BTreeMap::new();
        name.insert("tool".to_owned(), 0);
        let docs = vec![TermDoc {
            id: "x".to_owned(),
            name,
            text: BTreeMap::new(),
        }];
        let error = match Index::from_terms(&docs, Options::default()) {
            Ok(_) => panic!("a zero frequency"),
            Err(err) => err,
        };
        assert_eq!(
            error.to_string(),
            "bm25: document \"x\": name term \"tool\": invalid frequency 0"
        );
    }

    #[test]
    fn from_terms_and_from_text_agree() {
        let by_text = index(Options::default());
        let by_terms = match Index::from_terms(
            &docs()
                .iter()
                .map(|doc| TermDoc {
                    id: doc.id.clone(),
                    name: frequencies(&doc.name),
                    text: frequencies(&doc.text),
                })
                .collect::<Vec<_>>(),
            Options::default(),
        ) {
            Ok(index) => index,
            Err(err) => panic!("index: {err}"),
        };
        for query in ["web", "resume", "file"] {
            assert_eq!(by_text.search(query, None), by_terms.search(query, None));
        }
    }

    fn frequencies(text: &str) -> BTreeMap<String, i32> {
        let mut counts = BTreeMap::new();
        for token in tokenize(text) {
            *counts.entry(token).or_insert(0) += 1;
        }
        counts
    }

    #[test]
    fn an_empty_index_and_an_empty_query_answer_nothing() {
        let empty = match Index::new(&[], Options::default()) {
            Ok(index) => index,
            Err(err) => panic!("index: {err}"),
        };
        assert!(empty.search("anything", None).is_empty());
        assert!(empty.is_empty());
        let index = index(Options::default());
        assert!(index.search("", None).is_empty());
        assert!(index.search("!!!", None).is_empty());
        assert_eq!(index.len(), 4);
    }

    #[test]
    fn doc_freq_counts_a_document_once_across_fields() {
        let docs = vec![
            Doc::new("a", "tool", "tool"),
            Doc::new("b", "tool", "other"),
            Doc::new("c", "x", "tool"),
        ];
        let index = match Index::new(&docs, Options::default()) {
            Ok(index) => index,
            Err(err) => panic!("index: {err}"),
        };
        assert_eq!(index.doc_freq("tool"), 3);
        assert_eq!(index.doc_freq("absent"), 0);
    }
}
