//! Replays the committed bm25 contract against the port.
//!
//! `rust/conformance/bm25-contract.json` is dumped from core by
//! `tools/conformance-oracle -bm25-contract` and checked in; the gate fails if
//! regenerating it produces anything else. This test is the other half, and it
//! is the one that makes the tokenizer's Unicode claims checkable at all: the
//! `scripts` rows are every range boundary the port spells out, and a range
//! that is one code point too wide shows up here as a token list that differs.
//!
//! Scores are compared with a tolerance rather than bit for bit, which is a
//! deliberate exception to how the other contracts are compared. The reason is
//! `math.Log`: Go computes it with its own implementation, the port with the
//! platform's, and the two can differ in the last bit. Ranking order is
//! compared exactly, because that is what a caller observes, and the tolerance
//! is tight enough (1e-12 relative) that any drift that could reorder two
//! results fails here.

use flowcraft_utils::bm25::{Doc, Index, Match, Options, TermDoc, tokenize};
use serde_json::Value;

const CONTRACT: &str = include_str!("../../../conformance/bm25-contract.json");

#[test]
fn the_bm25_contract_replays() {
    let document = contract();
    let mut failures = Vec::new();

    replay_tokenize(&document, &mut failures);
    replay_scripts(&document, &mut failures);
    replay_searches(&document, "searches", &mut failures);
    replay_searches(&document, "term_searches", &mut failures);
    replay_configs(&document, &mut failures);
    replay_frequencies(&document, &mut failures);

    if !failures.is_empty() {
        panic!(
            "{} of the committed samples do not replay:\n{}",
            failures.len(),
            failures[..failures.len().min(40)].join("\n")
        );
    }
}

fn contract() -> Value {
    match serde_json::from_str(CONTRACT) {
        Ok(document) => document,
        Err(err) => panic!("the committed contract does not decode: {err}"),
    }
}

fn rows<'a>(document: &'a Value, table: &str) -> &'a [Value] {
    match document[table].as_array() {
        Some(rows) => rows,
        None => panic!("{table} is not an array"),
    }
}

fn replay_tokenize(document: &Value, failures: &mut Vec<String>) {
    for row in rows(document, "tokenize") {
        let input = string(row, "input");
        let expected = strings(row, "tokens");
        let got = tokenize(input);
        if got != expected {
            failures.push(format!(
                "tokenize {input:?}: port {got:?}, core {expected:?}"
            ));
        }
    }
}

fn replay_scripts(document: &Value, failures: &mut Vec<String>) {
    for row in rows(document, "scripts") {
        let code = match row["code"].as_u64() {
            Some(code) => code,
            None => {
                failures.push(format!("script row without a code: {row}"));
                continue;
            }
        };
        let character = match char::from_u32(code as u32) {
            Some(character) => character,
            None => continue,
        };
        let single = tokenize(&character.to_string());
        let double = tokenize(&format!("{character}{character}"));
        let expected_single = strings(row, "single");
        let expected_double = strings(row, "double");
        if single != expected_single {
            failures.push(format!(
                "script {code:#x} single: port {single:?}, core {expected_single:?}"
            ));
        }
        if double != expected_double {
            failures.push(format!(
                "script {code:#x} double: port {double:?}, core {expected_double:?}"
            ));
        }
    }
}

fn replay_searches(document: &Value, table: &str, failures: &mut Vec<String>) {
    for (position, row) in rows(document, table).iter().enumerate() {
        let where_ = format!("{table}[{position}]");
        let options = options_of(row);
        let docs: Vec<Doc> = match row["docs"].as_array() {
            Some(docs) => docs
                .iter()
                .map(|doc| Doc::new(string(doc, "id"), string(doc, "name"), string(doc, "text")))
                .collect(),
            None => Vec::new(),
        };
        let index = Index::new(&docs, options);
        let error = string(row, "error");
        if !error.is_empty() {
            match index {
                Ok(_) => failures.push(format!("{where_}: port indexed a refused corpus")),
                Err(err) if err.to_string() == error => {}
                Err(err) => failures.push(format!("{where_}: port said {err:?}, core {error:?}")),
            }
            continue;
        }
        let index = match index {
            Ok(index) => index,
            Err(err) => {
                failures.push(format!(
                    "{where_}: port refused with {err:?}, core accepted"
                ));
                continue;
            }
        };
        let limit = match row["limit"].as_i64().unwrap_or(0) {
            limit if limit <= 0 => None,
            limit => Some(limit as usize),
        };
        let got = if row["terms"].is_array() {
            index.search_terms(&strings(row, "terms"), limit)
        } else {
            index.search(string(row, "query"), limit)
        };
        compare_hits(&where_, &got, row, failures);
    }
}

fn replay_configs(document: &Value, failures: &mut Vec<String>) {
    for (position, row) in rows(document, "configs").iter().enumerate() {
        let where_ = format!("configs[{position}]");
        let options = options_of(row);
        let docs = [Doc::new("x", "a", "b")];
        let built = Index::new(&docs, options);
        let error = string(row, "error");
        if error.is_empty() {
            if let Err(err) = built {
                failures.push(format!("{where_}: port refused {err:?}, core accepted"));
            }
            continue;
        }
        match built {
            Ok(_) => failures.push(format!("{where_}: port accepted, core said {error:?}")),
            Err(err) if err.to_string() == error => {}
            Err(err) => failures.push(format!("{where_}: port said {err:?}, core {error:?}")),
        }
    }
}

fn replay_frequencies(document: &Value, failures: &mut Vec<String>) {
    for (position, row) in rows(document, "frequencies").iter().enumerate() {
        let where_ = format!("frequencies[{position}]");
        let terms = TermDoc {
            id: string(row, "id").to_owned(),
            name: frequencies(row, "name"),
            text: frequencies(row, "text"),
        };
        let built = Index::from_terms(&[terms], Options::default());
        let error = string(row, "error");
        if !error.is_empty() {
            match built {
                Ok(_) => failures.push(format!("{where_}: port indexed a refused document")),
                Err(err) if err.to_string() == error => {}
                Err(err) => failures.push(format!("{where_}: port said {err:?}, core {error:?}")),
            }
            continue;
        }
        let index = match built {
            Ok(index) => index,
            Err(err) => {
                failures.push(format!(
                    "{where_}: port refused with {err:?}, core accepted"
                ));
                continue;
            }
        };
        let got = index.search(string(row, "query"), None);
        compare_hits(&where_, &got, row, failures);
    }
}

fn compare_hits(where_: &str, got: &[Match], row: &Value, failures: &mut Vec<String>) {
    let expected = match row["results"].as_array() {
        Some(results) => results,
        None => {
            if !got.is_empty() {
                failures.push(format!(
                    "{where_}: port matched {} documents, core none",
                    got.len()
                ));
            }
            return;
        }
    };
    let got_ids: Vec<&str> = got.iter().map(|hit| hit.id.as_str()).collect();
    let expected_ids: Vec<&str> = expected.iter().map(|hit| string(hit, "id")).collect();
    if got_ids != expected_ids {
        failures.push(format!(
            "{where_}: port ranked {got_ids:?}, core {expected_ids:?}"
        ));
        return;
    }
    for (hit, want) in got.iter().zip(expected) {
        let want_score: f64 = match string(want, "score").parse() {
            Ok(score) => score,
            Err(err) => {
                failures.push(format!("{where_}: core's score does not parse: {err}"));
                continue;
            }
        };
        let delta = (hit.score - want_score).abs();
        let tolerance = want_score.abs() * 1e-12;
        if delta > tolerance {
            failures.push(format!(
                "{where_}: {} scored {} , core {} (delta {delta:e})",
                hit.id, hit.score, want_score
            ));
        }
    }
}

fn options_of(row: &Value) -> Options {
    let mut options = Options::default();
    if let Some(k1) = row["k1"].as_f64() {
        options = options.with_k1(k1);
    }
    if let Some(b) = row["b"].as_f64() {
        options = options.with_b(b);
    }
    if let Some(weight) = row["prefix_weight"].as_f64() {
        options = options.with_prefix_weight(weight);
    }
    options
}

fn frequencies(row: &Value, field: &str) -> std::collections::BTreeMap<String, i32> {
    let mut terms = std::collections::BTreeMap::new();
    if let Some(map) = row[field].as_object() {
        for (term, count) in map {
            terms.insert(term.clone(), count.as_i64().unwrap_or(0) as i32);
        }
    }
    terms
}

fn string<'a>(row: &'a Value, field: &str) -> &'a str {
    row[field].as_str().unwrap_or_default()
}

fn strings(row: &Value, field: &str) -> Vec<String> {
    row[field]
        .as_array()
        .map(|values| {
            values
                .iter()
                .map(|value| value.as_str().unwrap_or_default().to_owned())
                .collect()
        })
        .unwrap_or_default()
}
