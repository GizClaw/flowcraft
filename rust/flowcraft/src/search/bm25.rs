//! A small in-memory BM25 index over a fixed document set.
//!
//! Ported from `core/utils/bm25`. The index is deliberately dependency-free:
//! a catalog of a few hundred to a few thousand short documents is exactly the
//! regime where a hand-rolled inverted index beats pulling in a search engine.
//!
//! A document carries a name (an identifier) and a text (its description). Name
//! hits score higher, and a query term also matches index terms by prefix, so a
//! fragment still surfaces longer identifiers. Tokens are lowercased word runs;
//! CJK scripts are indexed as single characters plus adjacent bigrams.

use std::collections::HashMap;

use crate::error::{Error, Result};

use crate::search::tokenize::tokenize;

const K1: f64 = 1.2;
const B: f64 = 0.75;
const NAME_BOOST: f64 = 3.0;
const TEXT_BOOST: f64 = 1.0;
const PREFIX_WEIGHT: f64 = 0.5;

const NAME_FIELD: usize = 0;
const TEXT_FIELD: usize = 1;
const FIELD_BOOSTS: [f64; 2] = [NAME_BOOST, TEXT_BOOST];

/// One indexed document with pre-tokenized fields.
#[derive(Debug, Clone, Default)]
pub struct TermDoc {
    /// Document id.
    pub id: String,
    /// Name-field term frequencies.
    pub name: HashMap<String, usize>,
    /// Text-field term frequencies.
    pub text: HashMap<String, usize>,
}

/// One ranked match.
#[derive(Debug, Clone, PartialEq)]
pub struct Hit {
    /// Document id.
    pub id: String,
    /// BM25 score.
    pub score: f64,
}

#[derive(Debug)]
struct TermEntry {
    term: String,
    offset: usize,
    document_frequency: usize,
}

#[derive(Debug, Clone, Copy)]
struct Posting {
    document: usize,
    frequency: usize,
}

#[derive(Debug)]
struct Pending {
    term: String,
    document: usize,
    frequency: usize,
}

#[derive(Debug, Default)]
struct FieldIndex {
    lengths: Vec<usize>,
    total_length: usize,
    average: f64,
    terms: Vec<TermEntry>,
    postings: Vec<Posting>,
    pending: Vec<Pending>,
}

impl FieldIndex {
    fn add_text(&mut self, document: usize, text: &str) {
        let mut tokens = tokenize(text);
        self.lengths.push(tokens.len());
        self.total_length += tokens.len();
        tokens.sort_unstable();
        let mut start = 0;
        while start < tokens.len() {
            let mut end = start + 1;
            while end < tokens.len() && tokens[end] == tokens[start] {
                end += 1;
            }
            self.pending.push(Pending {
                term: std::mem::take(&mut tokens[start]),
                document,
                frequency: end - start,
            });
            start = end;
        }
    }

    fn add_terms(
        &mut self,
        document: usize,
        id: &str,
        terms: &HashMap<String, usize>,
    ) -> Result<()> {
        let mut total = 0;
        for (term, frequency) in terms {
            if term.is_empty() {
                return Err(Error::validation(format!(
                    "bm25: document {id:?} has an empty term"
                )));
            }
            if *frequency == 0 {
                return Err(Error::validation(format!(
                    "bm25: document {id:?}: term {term:?} has an invalid frequency"
                )));
            }
            total += frequency;
            self.pending.push(Pending {
                term: term.clone(),
                document,
                frequency: *frequency,
            });
        }
        self.lengths.push(total);
        self.total_length += total;
        Ok(())
    }

    fn finalize(&mut self) {
        let mut pending = std::mem::take(&mut self.pending);
        pending.sort_by(|a, b| {
            a.term
                .cmp(&b.term)
                .then_with(|| a.document.cmp(&b.document))
        });
        for item in pending {
            match self.terms.last_mut() {
                Some(entry) if entry.term == item.term => {
                    entry.document_frequency += 1;
                }
                _ => self.terms.push(TermEntry {
                    term: item.term,
                    offset: self.postings.len(),
                    document_frequency: 1,
                }),
            }
            self.postings.push(Posting {
                document: item.document,
                frequency: item.frequency,
            });
        }
    }

    fn position_of(&self, term: &str) -> usize {
        self.terms
            .binary_search_by(|entry| entry.term.as_str().cmp(term))
            .unwrap_or_else(|position| position)
    }

    fn postings_of(&self, term: &str) -> &[Posting] {
        let position = self.position_of(term);
        match self.terms.get(position) {
            Some(entry) if entry.term == term => {
                &self.postings[entry.offset..entry.offset + entry.document_frequency]
            }
            _ => &[],
        }
    }

    fn each_prefix(&self, prefix: &str, mut visit: impl FnMut(&str)) {
        for entry in &self.terms[self.position_of(prefix)..] {
            if !entry.term.starts_with(prefix) {
                return;
            }
            if entry.term.len() > prefix.len() {
                visit(&entry.term);
            }
        }
    }
}

/// An immutable BM25 index.
#[derive(Debug)]
pub struct Index {
    ids: Vec<String>,
    fields: [FieldIndex; 2],
    documents: usize,
}

impl Index {
    /// Build an index over pre-tokenized documents.
    pub fn from_terms(documents: &[TermDoc]) -> Result<Self> {
        let mut index = Self {
            ids: Vec::with_capacity(documents.len()),
            fields: [FieldIndex::default(), FieldIndex::default()],
            documents: documents.len(),
        };
        let mut seen = std::collections::BTreeSet::new();
        for (position, document) in documents.iter().enumerate() {
            if !seen.insert(document.id.clone()) {
                return Err(Error::validation(format!(
                    "bm25: duplicate document id {:?}",
                    document.id
                )));
            }
            index.ids.push(document.id.clone());
            index.fields[NAME_FIELD].add_terms(position, &document.id, &document.name)?;
            index.fields[TEXT_FIELD].add_terms(position, &document.id, &document.text)?;
        }
        index.finish();
        Ok(index)
    }

    /// Build an index over raw documents.
    pub fn new(documents: &[(String, String)]) -> Result<Self> {
        let mut index = Self {
            ids: Vec::with_capacity(documents.len()),
            fields: [FieldIndex::default(), FieldIndex::default()],
            documents: documents.len(),
        };
        let mut seen = std::collections::BTreeSet::new();
        for (position, (id, text)) in documents.iter().enumerate() {
            if !seen.insert(id.clone()) {
                return Err(Error::validation(format!(
                    "bm25: duplicate document id {id:?}"
                )));
            }
            index.ids.push(id.clone());
            index.fields[NAME_FIELD].add_text(position, id);
            index.fields[TEXT_FIELD].add_text(position, text);
        }
        index.finish();
        Ok(index)
    }

    fn finish(&mut self) {
        for field in &mut self.fields {
            field.finalize();
            if self.documents > 0 {
                field.average = field.total_length as f64 / self.documents as f64;
            }
        }
    }

    /// Rank documents against a raw query; `limit <= 0` means no limit.
    #[must_use]
    pub fn search(&self, query: &str, limit: usize) -> Vec<Hit> {
        self.search_terms(&unique_terms(tokenize(query)), limit)
    }

    /// Rank documents against already-tokenized query terms.
    #[must_use]
    pub fn search_terms(&self, query: &[String], limit: usize) -> Vec<Hit> {
        let terms = unique_terms(query.to_vec());
        if terms.is_empty() || self.documents == 0 {
            return Vec::new();
        }
        let mut scores = vec![0.0_f64; self.documents];
        let mut best = vec![0.0_f64; self.documents];
        for group in self.term_matches(&terms) {
            let mut touched: Vec<usize> = Vec::new();
            for (field, boost) in FIELD_BOOSTS.iter().enumerate() {
                let index = &self.fields[field];
                if index.average == 0.0 {
                    continue;
                }
                for item in &group {
                    let weight = if item.prefix {
                        boost * PREFIX_WEIGHT
                    } else {
                        *boost
                    };
                    let idf = self.inverse_document_frequency(item.document_frequency);
                    for posting in index.postings_of(&item.term) {
                        let frequency = posting.frequency as f64;
                        let denominator = frequency
                            + K1 * (1.0 - B
                                + B * index.lengths[posting.document] as f64 / index.average);
                        let score = weight * idf * frequency * (K1 + 1.0) / denominator;
                        if score > best[posting.document] {
                            if best[posting.document] == 0.0 {
                                touched.push(posting.document);
                            }
                            best[posting.document] = score;
                        }
                    }
                }
                for document in &touched {
                    scores[*document] += best[*document];
                    best[*document] = 0.0;
                }
                touched.clear();
            }
        }

        let mut ranked: Vec<Hit> = scores
            .iter()
            .enumerate()
            .filter(|(_, score)| **score > 0.0)
            .map(|(document, score)| Hit {
                id: self.ids[document].clone(),
                score: *score,
            })
            .collect();
        ranked.sort_by(|a, b| {
            b.score
                .partial_cmp(&a.score)
                .unwrap_or(std::cmp::Ordering::Equal)
        });
        if limit > 0 {
            ranked.truncate(limit);
        }
        ranked
    }

    fn inverse_document_frequency(&self, document_frequency: usize) -> f64 {
        let documents = self.documents as f64;
        let frequency = document_frequency as f64;
        (1.0 + (documents - frequency + 0.5) / (frequency + 0.5)).ln()
    }

    fn term_matches(&self, terms: &[String]) -> Vec<Vec<Match>> {
        let mut groups: Vec<Vec<Match>> = Vec::with_capacity(terms.len());
        let mut claimed: HashMap<String, (usize, usize)> = HashMap::new();
        for term in terms {
            let mut group = Vec::new();
            let mut add = |candidate: &str, prefix: bool, groups: &mut Vec<Vec<Match>>| {
                if let Some((group, at)) = claimed.get(candidate).copied() {
                    if !prefix {
                        groups[group][at].prefix = false;
                    }
                    return;
                }
                claimed.insert(candidate.to_owned(), (groups.len(), group.len()));
                group.push(Match {
                    term: candidate.to_owned(),
                    document_frequency: self.document_frequency(candidate),
                    prefix,
                });
            };
            add(term, false, &mut groups);
            for field in &self.fields {
                field.each_prefix(term, |candidate| add(candidate, true, &mut groups));
            }
            groups.push(group);
        }
        groups
    }

    fn document_frequency(&self, term: &str) -> usize {
        let name = self.fields[NAME_FIELD].postings_of(term);
        let text = self.fields[TEXT_FIELD].postings_of(term);
        match (name.is_empty(), text.is_empty()) {
            (true, _) => text.len(),
            (_, true) => name.len(),
            _ => {
                let (mut left, mut right, mut count) = (0, 0, 0);
                while left < name.len() && right < text.len() {
                    if name[left].document == text[right].document {
                        left += 1;
                        right += 1;
                    } else if name[left].document < text[right].document {
                        left += 1;
                    } else {
                        right += 1;
                    }
                    count += 1;
                }
                count + (name.len() - left) + (text.len() - right)
            }
        }
    }
}

#[derive(Debug)]
struct Match {
    term: String,
    document_frequency: usize,
    prefix: bool,
}

fn unique_terms(terms: Vec<String>) -> Vec<String> {
    let mut seen = std::collections::BTreeSet::new();
    terms
        .into_iter()
        .filter(|term| !term.is_empty() && seen.insert(term.clone()))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn an_exact_term_outranks_a_prefix_match() {
        let index = Index::new(&[
            ("resume".to_owned(), "resume a task".to_owned()),
            ("reset".to_owned(), "reset the state".to_owned()),
        ])
        .expect("index");
        let hits = index.search("resume", 8);
        assert_eq!(hits.first().map(|hit| hit.id.as_str()), Some("resume"));
    }

    #[test]
    fn a_name_hit_outranks_a_text_hit() {
        let index = Index::new(&[
            ("grep".to_owned(), "unrelated".to_owned()),
            ("other".to_owned(), "grep lines".to_owned()),
        ])
        .expect("index");
        let hits = index.search("grep", 8);
        assert_eq!(hits[0].id, "grep");
    }

    #[test]
    fn ties_keep_document_order() {
        let index = Index::new(&[
            ("b".to_owned(), "same".to_owned()),
            ("a".to_owned(), "same".to_owned()),
        ])
        .expect("index");
        let hits = index.search("same", 8);
        assert_eq!(
            hits.iter().map(|hit| hit.id.as_str()).collect::<Vec<_>>(),
            vec!["b", "a"]
        );
    }

    #[test]
    fn an_empty_query_or_corpus_returns_nothing() {
        let index = Index::new(&[]).expect("index");
        assert!(index.search("anything", 8).is_empty());
        let index = Index::new(&[("a".to_owned(), "b".to_owned())]).expect("index");
        assert!(index.search("   ", 8).is_empty());
    }
}
