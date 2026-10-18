//! Replays the committed pathsafe contract against the port.
//!
//! `rust/conformance/pathsafe-contract.json` is dumped from core by
//! `tools/conformance-oracle -pathsafe-contract` and checked in; the gate fails
//! if regenerating it produces anything else. This test is the other half.
//!
//! The rows exist because the three functions look trivial and are not:
//! `pathsafe.Rel` walks up and down and *then* refuses a target outside the
//! root, `filepath.Clean` folds an inner `..` while keeping a relative leading
//! one, and `ResolveUnder` appends an absolute reference instead of letting it
//! replace the root (`filepath.Join`, not `Path::join`). Each of those is a
//! place where a plausible Rust spelling answers the opposite of core, so each
//! has rows of its own.
//!
//! The filesystem half is not here -- `RealWithin`/`RealDir` depend on a tree,
//! so they are tested where the tree is built (`src/pathsafe.rs`).

use std::path::Path;

use flowcraft_utils::pathsafe::{self, Error};
use serde_json::Value;

const CONTRACT: &str = include_str!("../../../conformance/pathsafe-contract.json");

#[test]
fn the_pathsafe_contract_replays() {
    let document = contract();
    let mut failures = Vec::new();

    replay_within(&document, &mut failures);
    replay_rel(&document, &mut failures);
    replay_resolve(&document, &mut failures);
    replay_rel_ref(&document, &mut failures);
    replay_clean(&document, &mut failures);

    if !failures.is_empty() {
        panic!(
            "{} of the committed samples do not replay:\n{}",
            failures.len(),
            failures.join("\n")
        );
    }
}

fn contract() -> Value {
    match serde_json::from_str(CONTRACT) {
        Ok(value) => value,
        Err(err) => panic!("the committed contract does not decode: {err}"),
    }
}

fn rows<'a>(document: &'a Value, table: &str) -> &'a Vec<Value> {
    match document[table].as_array() {
        Some(rows) => rows,
        None => panic!("{table} is not an array"),
    }
}

fn replay_within(document: &Value, failures: &mut Vec<String>) {
    for row in rows(document, "within") {
        let root = text(row, "root");
        let path = text(row, "path");
        let where_ = format!("within({root:?}, {path:?})");
        let expected = flag(row, "within");
        let got = pathsafe::within(root, path);
        if got != expected {
            failures.push(format!("{where_}: port says {got}, core {expected}"));
        }
        let expected = flag(row, "within_fold");
        let got = pathsafe::within_fold(root, path);
        if got != expected {
            failures.push(format!("{where_} fold: port says {got}, core {expected}"));
        }
    }
}

fn replay_rel(document: &Value, failures: &mut Vec<String>) {
    for row in rows(document, "rel") {
        let root = text(row, "root");
        let target = text(row, "target");
        let where_ = format!("rel({root:?}, {target:?})");
        let expected = if flag(row, "ok") {
            Some(text(row, "rel").to_owned())
        } else {
            None
        };
        let got = pathsafe::rel(root, target);
        match (got, expected) {
            (Some(got), Some(expected)) if got == expected => {}
            (Some(got), Some(expected)) => {
                failures.push(format!("{where_}: port says {got:?}, core {expected:?}"));
            }
            (None, None) => {}
            (Some(got), None) => {
                failures.push(format!("{where_}: port says {got:?}, core refused"));
            }
            (None, Some(expected)) => {
                failures.push(format!("{where_}: port refused, core says {expected:?}"));
            }
        }
    }
}

fn replay_resolve(document: &Value, failures: &mut Vec<String>) {
    for row in rows(document, "resolve") {
        let root = text(row, "root");
        let reference = text(row, "reference");
        let where_ = format!("resolve_under({root:?}, {reference:?})");
        match pathsafe::resolve_under(root, reference) {
            Ok(path) => {
                let expected = text(row, "path");
                if expected.is_empty() {
                    failures.push(format!(
                        "{where_}: port resolved to {}, core refused with {:?}",
                        path.display(),
                        text(row, "error")
                    ));
                    continue;
                }
                if path != Path::new(expected) {
                    failures.push(format!(
                        "{where_}: port resolved to {}, core {expected:?}",
                        path.display()
                    ));
                }
            }
            Err(error) => {
                let expected = text(row, "error");
                if expected.is_empty() {
                    failures.push(format!(
                        "{where_}: port refused with {error}, core resolved to {:?}",
                        text(row, "path")
                    ));
                } else if error.to_string() != expected {
                    failures.push(format!("{where_}: port said {error:?}, core {expected:?}"));
                }
            }
        }
    }
}

fn replay_rel_ref(document: &Value, failures: &mut Vec<String>) {
    for row in rows(document, "rel_ref") {
        let reference = text(row, "reference");
        let expected = flag(row, "ok");
        let got = pathsafe::rel_ref(reference);
        if got != expected {
            failures.push(format!(
                "rel_ref({reference:?}): port says {got}, core {expected}"
            ));
        }
    }
}

fn replay_clean(document: &Value, failures: &mut Vec<String>) {
    for row in rows(document, "clean") {
        let input = text(row, "input");
        let expected = text(row, "cleaned");
        let got = pathsafe::clean(input);
        if got != Path::new(expected) {
            failures.push(format!(
                "clean({input:?}): port says {}, core {expected:?}",
                got.display()
            ));
        }
    }
}

/// The contract has to keep covering the refusals: a dump that lost its
/// escaping rows would still replay, and every sample in it would pass.
#[test]
fn the_contract_still_covers_the_refusals() {
    let document = contract();
    let refused = rows(&document, "rel")
        .iter()
        .filter(|row| !flag(row, "ok"))
        .count();
    assert!(
        refused >= 10,
        "rel refused {refused} rows; it used to be more"
    );
    let escapes = rows(&document, "resolve")
        .iter()
        .filter(|row| text(row, "error").contains("escapes"))
        .count();
    assert!(escapes >= 3, "resolve refused {escapes} escapes");
    let climbs = rows(&document, "rel_ref")
        .iter()
        .filter(|row| !flag(row, "ok"))
        .count();
    assert!(climbs >= 5, "rel_ref refused {climbs} references");
}

fn text<'a>(row: &'a Value, field: &str) -> &'a str {
    row[field].as_str().unwrap_or_default()
}

fn flag(row: &Value, field: &str) -> bool {
    row[field].as_bool().unwrap_or(false)
}

/// The refusals this module makes are part of the contract, so their shape is
/// asserted once here: an escaping reference names both sides and quotes the
/// reference the way Go's `%q` does.
#[test]
fn an_escape_names_both_sides() {
    let error = match pathsafe::resolve_under("/w/root", "../escape") {
        Ok(path) => panic!("the reference resolved to {}", path.display()),
        Err(error) => error,
    };
    assert_eq!(
        error,
        Error::Escapes {
            reference: "../escape".to_owned(),
            root: "/w/root".to_owned(),
        }
    );
    assert_eq!(error.to_string(), "pathsafe: \"../escape\" escapes /w/root");
}
