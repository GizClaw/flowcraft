//! Replays the committed filetype contract against the port.
//!
//! `rust/conformance/filetype-contract.json` is dumped from core by
//! `tools/conformance-oracle -filetype-contract` and checked in; the gate fails
//! if regenerating it produces anything else. This test is the other half.
//!
//! The corpus crosses every signature in the WHATWG table, every mask, the
//! html tag-terminating rule, the mp4 box walk (five ways to fail it), the
//! UTF-16/8 byte-order marks, and the 512-byte cutoff -- a signature that ends
//! exactly at the boundary is seen, one that starts there is not. Those are the
//! rows where an off-by-one is a silent misclassification rather than an error.
//!
//! `classify` rows use a name with no extension, so the ordered rule is pinned
//! without the host's MIME database: a text payload is text whatever the
//! sniffer says, a binary payload takes the sniffer's answer. Name-looking
//! behaviour is tested in `src/filetype.rs`, against the table rather than
//! against a host.

use base64::Engine as _;
use flowcraft_utils::filetype;
use serde_json::Value;

const CONTRACT: &str = include_str!("../../../conformance/filetype-contract.json");

#[test]
fn the_filetype_contract_replays() {
    let document = contract();
    let mut failures = Vec::new();

    for row in rows(&document, "sniffs") {
        let data = sample(row);
        let expected = text(row, "type");
        let got = filetype::detect_content_type(&data);
        if got != expected {
            failures.push(format!(
                "detect({}) = {got:?}, core {expected:?}",
                describe(&data)
            ));
        }
    }
    for row in rows(&document, "classify") {
        let data = sample(row);
        let expected = (
            text(row, "media_type"),
            row["text"].as_bool().unwrap_or(false),
        );
        let kind = filetype::of_data("blob", &data);
        if (kind.media_type.as_str(), kind.text) != expected {
            failures.push(format!(
                "of_data({}) = ({:?}, {}), core {:?}",
                describe(&data),
                kind.media_type,
                kind.text,
                expected
            ));
        }
    }

    if !failures.is_empty() {
        panic!(
            "{} of the committed samples do not replay:\n{}",
            failures.len(),
            failures.join("\n")
        );
    }
}

/// The corpus has to keep covering what it claims: a table that lost its
/// signature rows would still replay.
#[test]
fn the_corpus_still_crosses_the_table() {
    let document = contract();
    let sniffs = rows(&document, "sniffs");
    assert!(
        sniffs.len() >= 70,
        "the corpus has only {} rows",
        sniffs.len()
    );
    let mut types: Vec<&str> = sniffs.iter().map(|row| text(row, "type")).collect();
    types.sort_unstable();
    types.dedup();
    assert!(
        types.len() >= 30,
        "the corpus reaches only {} distinct answers",
        types.len()
    );
    // The boundary rows are the ones worth naming: 512 bytes is the last byte
    // the sniffer reads. A signature that ends exactly there is seen ...
    let png = [0x89, b'P', b'N', b'G', 0x0d, 0x0a, 0x1a, 0x0a];
    let ending_at_the_cutoff = sniffs
        .iter()
        .any(|row| sample(row).len() == filetype::SNIFF_LEN && sample(row).ends_with(&png));
    assert!(
        ending_at_the_cutoff,
        "the cutoff rows are not in the corpus"
    );
    assert!(
        sniffs
            .iter()
            .any(|row| sample(row).len() > filetype::SNIFF_LEN),
        "no sample crosses the cutoff"
    );
    // ... and one that starts two bytes before it is cut in half and not seen
    // at all, which is the difference a `<=` would erase.
    let straddling = sniffs
        .iter()
        .find(|row| {
            let data = sample(row);
            data.len() == filetype::SNIFF_LEN + 4 && data.ends_with(&png)
        })
        .map(|row| text(row, "type"));
    assert_eq!(
        straddling,
        Some("text/plain; charset=utf-8"),
        "a signature past the cutoff was seen"
    );
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

/// A row's sample: the base64 string Go writes for a `[]byte`.
fn sample(row: &Value) -> Vec<u8> {
    match text(row, "data") {
        "" => Vec::new(),
        encoded => match base64::engine::general_purpose::STANDARD.decode(encoded) {
            Ok(bytes) => bytes,
            Err(err) => panic!("a sample does not decode: {err}"),
        },
    }
}

/// A sample in a failure message: short samples as bytes, long ones by shape.
fn describe(data: &[u8]) -> String {
    if data.len() <= 24 {
        return format!("{data:?}");
    }
    format!("{} bytes starting {:?}", data.len(), &data[..12])
}

fn text<'a>(row: &'a Value, field: &str) -> &'a str {
    row[field].as_str().unwrap_or_default()
}
