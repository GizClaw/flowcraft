//! The catalog-level search layer over [`flowcraft_search::bm25`].

use flowcraft_error::Result;
use serde::Serialize;

use flowcraft_search::{bm25, tokenize};

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

/// Rank catalog entries against a query.
///
/// Documents are indexed by name, so ties break by name. `limit == 0` means the
/// default.
pub fn bm25_search(docs: &[(String, String)], query: &str, limit: usize) -> Result<Vec<SearchHit>> {
    let query_terms = catalog_terms(query);
    if docs.is_empty() || query_terms.is_empty() {
        return Ok(Vec::new());
    }
    let mut indexed: Vec<bm25::TermDoc> = docs
        .iter()
        .map(|(name, description)| bm25::TermDoc {
            id: name.clone(),
            name: frequencies(catalog_terms(name)),
            text: frequencies(catalog_terms(description)),
        })
        .collect();
    indexed.sort_by(|a, b| a.id.cmp(&b.id));
    let index = bm25::Index::from_terms(&indexed)?;
    let limit = if limit == 0 {
        DEFAULT_SEARCH_LIMIT
    } else {
        limit
    };
    let descriptions: std::collections::HashMap<&str, &str> = docs
        .iter()
        .map(|(name, description)| (name.as_str(), description.as_str()))
        .collect();
    Ok(index
        .search_terms(&query_terms, limit)
        .into_iter()
        .map(|hit| SearchHit {
            description: descriptions
                .get(hit.id.as_str())
                .map(|description| description.trim().to_owned())
                .unwrap_or_default(),
            name: hit.id,
            score: hit.score,
        })
        .collect())
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
