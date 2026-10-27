//! The catalog-level search layer over [`crate::search::bm25`].

use crate::error::Result;
use serde::Serialize;

use crate::search::{bm25, tokenize};

/// One `tool_search` hit.
#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct SearchHit {
    /// Tool name.
    pub name: String,
    /// Tool description.
    pub description: String,
    /// BM25 score.
    pub score: f64,
}

/// Used when `tool_search` omits `limit`.
pub const DEFAULT_SEARCH_LIMIT: usize = 8;

/// A catalog indexed once, searchable many times.
///
/// The split between "build" and "query" is the point of this type. A catalog's
/// documents only change when the catalog does, while a query arrives on every
/// `tool_search` call: keeping the two in one function (`bm25_search`) meant the
/// tokenizing, the term-frequency maps and the inverted index were rebuilt for
/// every query — tens of thousands of small allocations for a catalog of a few
/// hundred tools.
#[derive(Debug)]
pub struct CatalogIndex {
    /// The documents, sorted by name, so a hit needs no separate lookup to name
    /// itself.
    docs: Vec<(String, String)>,
    /// `None` for an empty catalog: `bm25` rejects an empty corpus, and there is
    /// nothing to search anyway.
    index: Option<bm25::Index>,
}

impl CatalogIndex {
    /// Index `docs` (name, description).
    pub fn new(mut docs: Vec<(String, String)>) -> Result<Self> {
        if docs.is_empty() {
            return Ok(Self { docs, index: None });
        }
        // One order for both uses below: `bm25` wants a deterministic corpus so
        // ties break by name, and `description` binary-searches `docs`.
        docs.sort_by(|(left, _), (right, _)| left.cmp(right));
        let indexed: Vec<bm25::TermDoc> = docs
            .iter()
            .map(|(name, description)| bm25::TermDoc {
                id: name.clone(),
                name: frequencies(catalog_terms(name)),
                text: frequencies(catalog_terms(description)),
            })
            .collect();
        Ok(Self {
            docs,
            index: Some(bm25::Index::from_terms(&indexed)?),
        })
    }

    /// The indexed documents, sorted by name.
    #[must_use]
    pub fn docs(&self) -> &[(String, String)] {
        &self.docs
    }

    /// Rank the indexed documents against `query`. `limit == 0` means the
    /// default.
    pub fn search(&self, query: &str, limit: usize) -> Result<Vec<SearchHit>> {
        let Some(index) = self.index.as_ref() else {
            return Ok(Vec::new());
        };
        let query_terms = catalog_terms(query);
        if query_terms.is_empty() {
            return Ok(Vec::new());
        }
        let limit = if limit == 0 {
            DEFAULT_SEARCH_LIMIT
        } else {
            limit
        };
        Ok(index
            .search_terms(&query_terms, limit)
            .into_iter()
            .map(|hit| SearchHit {
                description: self
                    .description(&hit.id)
                    .map(|description| description.trim().to_owned())
                    .unwrap_or_default(),
                name: hit.id,
                score: hit.score,
            })
            .collect())
    }

    fn description(&self, name: &str) -> Option<&str> {
        self.docs
            .binary_search_by(|(candidate, _)| candidate.as_str().cmp(name))
            .ok()
            .map(|at| self.docs[at].1.as_str())
    }
}

/// Rank catalog entries against a query, indexing them first.
///
/// Convenience for a one-shot search over a document set that is about to be
/// discarded: it is [`CatalogIndex::new`] followed by [`CatalogIndex::search`].
/// A caller that searches the same catalog more than once wants the index.
///
/// Documents are indexed by name, so ties break by name. `limit == 0` means the
/// default.
pub fn bm25_search(docs: &[(String, String)], query: &str, limit: usize) -> Result<Vec<SearchHit>> {
    CatalogIndex::new(docs.to_vec())?.search(query, limit)
}

/// Tokenize, then drop single-rune ASCII terms.
///
/// A bare `a` or `1` matches almost everything and expands to every term with
/// that prefix; single CJK characters stay, because they are whole words.
fn catalog_terms(value: &str) -> Vec<String> {
    tokenize(value)
        .into_iter()
        .filter(|term| !(term.len() == 1 && term.is_ascii()))
        .collect()
}

fn frequencies(tokens: Vec<String>) -> std::collections::HashMap<String, usize> {
    let mut counts = std::collections::HashMap::new();
    for token in tokens {
        *counts.entry(token).or_insert(0) += 1;
    }
    counts
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ranks_the_most_relevant_tool_first() {
        let docs = vec![
            (
                "read".to_owned(),
                "read a file from the workspace".to_owned(),
            ),
            (
                "grep".to_owned(),
                "search file contents with a pattern".to_owned(),
            ),
            ("clock".to_owned(), "tell the current time".to_owned()),
        ];
        let hits = bm25_search(&docs, "search file pattern", 8).unwrap();
        assert_eq!(hits[0].name, "grep");
    }

    #[test]
    fn an_empty_query_or_catalog_returns_nothing() {
        assert!(bm25_search(&[], "anything", 8).unwrap().is_empty());
        let docs = vec![("a".to_owned(), "b".to_owned())];
        assert!(bm25_search(&docs, "  ", 8).unwrap().is_empty());
    }

    #[test]
    fn a_single_ascii_letter_is_not_a_term() {
        assert!(catalog_terms("a").is_empty());
        assert_eq!(catalog_terms("ab"), vec!["ab"]);
    }
}
