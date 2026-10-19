//! The skill contract: every `SKILL.md` core's parser is asked about, replayed.
//!
//! The rows are files written into a temporary directory of this test's own, so
//! the one thing that cannot be compared as-is is that directory's absolute path
//! — masked out of every string the parser returns, `metadata.path` included.
//! Two more strings are masked, both of them somebody else's wording: an
//! operating system's sentence after the verb and path core wrote (`stat
//! absent/SKILL.md: <os error>`), and a YAML library's error text after the
//! sentence that classifies it (`skill: parse frontmatter: <decode error>`). What
//! is compared is everything core chose: the metadata, the warnings, and the
//! classification sentence of a refusal.

use std::fs;
use std::path::{Path, PathBuf};

use flowcraft_skill::{
    MAX_DESCRIPTION_LEN, MAX_NAME_LEN, MAX_SKILL_FILE_BYTES, Metadata, parse_file,
};
use serde::Deserialize;

/// The committed document.
#[derive(Deserialize)]
struct Contract {
    /// The caps the dump recorded, from core.
    limits: Limits,
    /// The files and their answers.
    rows: Vec<Row>,
}

/// Every cap a parse applies.
#[derive(Deserialize)]
struct Limits {
    /// The maximum name length in runes.
    name: usize,
    /// The maximum description length in runes.
    description: usize,
    /// The maximum `SKILL.md` size in bytes.
    file: u64,
}

/// One file and the answer core gave it.
#[derive(Deserialize)]
struct Row {
    /// Identifies the row in a mismatch.
    name: String,
    /// What the row is evidence of.
    why: String,
    /// Where the file is written, relative to this test's directory.
    path: String,
    /// `dir` when the row needs a directory where a file would be.
    #[serde(default)]
    kind: String,
    /// The bytes, when they are UTF-8.
    #[serde(default)]
    data: String,
    /// The bytes, when they are not.
    #[serde(default)]
    data_hex: String,
    /// The parse's answer, when it produced one.
    metadata: Option<Metadata>,
    /// What it accepted with a note.
    #[serde(default)]
    warnings: Vec<String>,
    /// What it refused.
    error: Option<String>,
}

/// What one replay of a row saw, in the same shape the dump records.
#[derive(Default)]
struct Answer {
    metadata: Option<Metadata>,
    warnings: Vec<String>,
    error: Option<String>,
}

#[test]
fn the_skill_contract_replays() {
    let contract: Contract = serde_json::from_str(
        &fs::read_to_string(contract_path()).unwrap_or_else(|err| panic!("{err}")),
    )
    .unwrap_or_else(|err| panic!("skill-contract.json: {err}"));

    // The dump records the caps it was generated with, so a port whose limits
    // drifted is a mismatch here rather than a mystery later.
    assert_eq!(contract.limits.name, MAX_NAME_LEN, "name cap");
    assert_eq!(
        contract.limits.description, MAX_DESCRIPTION_LEN,
        "description cap"
    );
    assert_eq!(contract.limits.file, MAX_SKILL_FILE_BYTES, "file cap");
    assert!(!contract.rows.is_empty(), "the contract has rows");

    let directory = Directory::new("skill-contract");
    let mut mismatches = Vec::new();
    for row in &contract.rows {
        let answer = replay(row, directory.path());
        let expected = Answer {
            metadata: row.metadata.clone(),
            warnings: row.warnings.clone(),
            error: row.error.clone(),
        };
        if let Some(reason) = difference(&expected, &answer) {
            mismatches.push(format!("row {}: {reason}\n  why: {}", row.name, row.why));
        }
    }
    assert!(
        mismatches.is_empty(),
        "{} of {} rows disagree:\n\n{}",
        mismatches.len(),
        contract.rows.len(),
        mismatches.join("\n\n")
    );
}

/// Runs one row: write the file, parse it, mask what cannot be compared.
fn replay(row: &Row, directory: &Path) -> Answer {
    let path = directory.join(&row.path);
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).unwrap_or_else(|err| panic!("{}: {err}", parent.display()));
    }
    if row.kind == "dir" {
        fs::create_dir_all(&path).unwrap_or_else(|err| panic!("{}: {err}", path.display()));
    } else if !row.data.is_empty() || !row.data_hex.is_empty() {
        let bytes = if row.data_hex.is_empty() {
            row.data.clone().into_bytes()
        } else {
            unhex(&row.data_hex)
        };
        fs::write(&path, bytes).unwrap_or_else(|err| panic!("{}: {err}", path.display()));
    }

    let text = directory.to_string_lossy();
    match parse_file(&path) {
        Ok(parsed) => Answer {
            metadata: Some(Metadata {
                path: mask(&parsed.metadata.path, &text),
                ..parsed.metadata
            }),
            warnings: parsed.warnings.iter().map(|w| mask(w, &text)).collect(),
            error: None,
        },
        Err(err) => Answer {
            metadata: None,
            warnings: Vec::new(),
            error: Some(mask(&err.to_string(), &text)),
        },
    }
}

/// The three masks the document lists, applied in the order the dump applied
/// them: the harness's directory, then an operating system's sentence, then a
/// library's error text.
fn mask(text: &str, directory: &str) -> String {
    let text = text.replace(&format!("{directory}/"), "");
    let text = text.replace(directory, ".");
    for verb in ["stat ", "read "] {
        if let Some(rest) = text.strip_prefix(verb)
            && let Some(at) = rest.find(": ")
        {
            return format!("{verb}{}<os error>", &rest[..at + 2]);
        }
    }
    const PREFIX: &str = "skill: parse frontmatter: ";
    if let Some(rest) = text.strip_prefix(PREFIX) {
        // The one sentence under this prefix that core writes itself: the keys
        // no JSON key can spell. Everything else is go-yaml's or
        // encoding/json's wording.
        if rest.starts_with("YAML key") {
            return text;
        }
        return format!("{PREFIX}<decode error>");
    }
    text
}

/// The first field two answers disagree about, as a sentence.
fn difference(expected: &Answer, actual: &Answer) -> Option<String> {
    match (&expected.error, &actual.error) {
        (Some(want), Some(got)) if want == got => {}
        (None, None) => {}
        (want, got) => {
            return Some(format!(
                "refusal\n  core: {}\n  port: {}",
                display(want.as_deref()),
                display(got.as_deref())
            ));
        }
    }
    match (&expected.metadata, &actual.metadata) {
        (Some(want), Some(got)) => {
            if want.name != got.name {
                return Some(format!("name: core {:?}, port {:?}", want.name, got.name));
            }
            if want.description != got.description {
                return Some(format!(
                    "description: core {:?}, port {:?}",
                    want.description, got.description
                ));
            }
            if want.short_description != got.short_description {
                return Some(format!(
                    "short_description: core {:?}, port {:?}",
                    want.short_description, got.short_description
                ));
            }
            if want.path != got.path {
                return Some(format!("path: core {:?}, port {:?}", want.path, got.path));
            }
        }
        (None, None) => {}
        (want, got) => {
            return Some(format!(
                "metadata\n  core: {}\n  port: {}",
                want.as_ref()
                    .map_or("none".to_owned(), |m| format!("{m:?}")),
                got.as_ref().map_or("none".to_owned(), |m| format!("{m:?}"))
            ));
        }
    }
    if expected.warnings != actual.warnings {
        return Some(format!(
            "warnings\n  core: {:?}\n  port: {:?}",
            expected.warnings, actual.warnings
        ));
    }
    None
}

/// A string, or `none` when the answer had none.
fn display(text: Option<&str>) -> String {
    text.unwrap_or("none").to_owned()
}

/// The committed dump, next to this crate in the workspace.
fn contract_path() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../../conformance/skill-contract.json")
}

/// Decodes the hex a row uses for bytes JSON cannot carry.
fn unhex(text: &str) -> Vec<u8> {
    let bytes = text.as_bytes();
    assert!(bytes.len().is_multiple_of(2), "odd hex length: {text}");
    bytes
        .chunks(2)
        .map(|pair| {
            let digit = |byte: u8| {
                (byte as char)
                    .to_digit(16)
                    .unwrap_or_else(|| panic!("not hex: {text}")) as u8
            };
            (digit(pair[0]) << 4) | digit(pair[1])
        })
        .collect()
}

/// A directory that removes itself, since the crate has no `tempfile`.
struct Directory {
    path: PathBuf,
}

impl Directory {
    fn new(name: &str) -> Directory {
        let path = std::env::temp_dir().join(format!("flowcraft-{name}-{}", std::process::id()));
        let _ = fs::remove_dir_all(&path);
        fs::create_dir_all(&path).unwrap_or_else(|err| panic!("{}: {err}", path.display()));
        Directory { path }
    }

    fn path(&self) -> &Path {
        &self.path
    }
}

impl Drop for Directory {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.path);
    }
}
