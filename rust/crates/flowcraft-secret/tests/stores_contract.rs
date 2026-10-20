//! The built-in stores' contract: both `secret.Store` implementations, their
//! strict settings decode, and what they answer when asked for a name.
//!
//! The settings in the committed document carry `$BASE` where a path would be;
//! both sides substitute it with their own temporary directory, so no path here
//! depends on the machine or on the run. A row with `error` is the factory
//! refusing the settings -- before any store exists -- and a row's `lookups` are
//! put to the store it built.
//!
//! Two rows are answered by the *process* rather than by the file: `PATH` is
//! whatever this machine has, so a `from_process` row compares the store's
//! answer against the replay's own environment rather than against a string
//! committed in the document.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use flowcraft_secret::{EnvSettings, FileSettings, SecretStore};
use serde::Deserialize;
use serde_json::{Map, Value};

/// One row of the committed document's `stores` table.
#[derive(Debug, Deserialize)]
struct StoreCase {
    /// Identifies the row in a mismatch.
    name: String,
    /// The implementation to build: `env` or `file`.
    #[serde(rename = "impl")]
    implementation: String,
    /// The settings subtree, with `$BASE` still in it.
    settings: Value,
    /// The medium a file-backed store reads, written before the store is built.
    #[serde(default)]
    files: Vec<StoreFile>,
    /// The store's reference id; `null` when it has none of its own.
    #[serde(default)]
    id: Option<String>,
    /// The store's default answer.
    #[serde(default)]
    default: bool,
    /// The factory's refusal, when the settings were refused.
    #[serde(default)]
    error: Option<String>,
    /// The factory's refusal up to the decoder's own words: a decode failure
    /// ends in a sentence `encoding/json` writes, and no port reproduces that;
    /// the sentence core wrote around it is compared as a prefix.
    #[serde(default)]
    error_prefix: Option<String>,
    /// The questions put to the built store.
    #[serde(default)]
    lookups: Vec<LookupAnswer>,
}

/// One file the row's medium holds.
#[derive(Debug, Deserialize)]
struct StoreFile {
    /// The path inside the base directory.
    name: String,
    /// The file's contents.
    #[serde(default)]
    data: String,
    /// Instead of `data`, that many `x` bytes -- a file whose size is the point.
    #[serde(default)]
    fill: i64,
}

/// What one lookup answered.
#[derive(Debug, Deserialize)]
struct LookupAnswer {
    /// The secret name asked about.
    name: String,
    /// The store's `found` return.
    #[serde(default)]
    found: bool,
    /// The value a found secret answered.
    #[serde(default)]
    value: String,
    /// The value is the process's environment: compare against this process.
    #[serde(default)]
    from_process: bool,
    /// The lookup's refusal, when it refused.
    #[serde(default)]
    error: Option<String>,
}

impl LookupAnswer {
    /// The row's name, as a mismatch reports it.
    fn why(&self) -> String {
        format!("lookup {:?}", self.name)
    }
}

#[test]
fn every_built_store_matches_the_contract() {
    let contract: BTreeMap<String, Value> = serde_json::from_str(DOCUMENT)
        .unwrap_or_else(|err| panic!("secret-contract.json is not an object: {err}"));
    let stores: Vec<StoreCase> = serde_json::from_value(
        contract
            .get("stores")
            .cloned()
            .unwrap_or_else(|| panic!("secret-contract.json has no stores table")),
    )
    .unwrap_or_else(|err| panic!("stores table: {err}"));
    assert!(
        !stores.is_empty(),
        "the stores table is empty, so this test would pass on nothing"
    );

    // The absent-variable row is an environment assumption, not a canned
    // answer: a machine that exports the name would make the row disagree with
    // the store for a reason that has nothing to do with the port.
    if std::env::var_os("FLOWCRAFT_CONTRACT_ABSENT").is_some() {
        panic!("FLOWCRAFT_CONTRACT_ABSENT is set in this environment; the contract needs it unset");
    }

    for row in &stores {
        let base = TempDir::new(&row.name);
        for file in &row.files {
            base.write(file);
        }
        let settings = substitute_base(&row.settings, base.path());
        check_row(row, &settings);
    }
}

/// Builds one row's store and checks everything the row claims about it.
fn check_row(row: &StoreCase, settings: &Value) {
    let built = build(&row.implementation, settings);
    let store = match (row.error.as_deref(), row.error_prefix.as_deref(), built) {
        (Some(expected), None, Err(actual)) => {
            assert_eq!(actual.to_string(), expected, "{}: refusal", row.name);
            return;
        }
        (None, Some(expected), Err(actual)) => {
            let actual = actual.to_string();
            let tail = actual.strip_prefix(expected).unwrap_or_else(|| {
                panic!("{}: {actual:?} does not start with {expected:?}", row.name)
            });
            assert!(
                !tail.is_empty(),
                "{}: the refusal is only its prefix, so the row proves nothing",
                row.name
            );
            return;
        }
        (Some(_), Some(_), _) => panic!("{}: the row carries both refusal forms", row.name),
        (Some(expected), None, Ok(_)) | (None, Some(expected), Ok(_)) => {
            panic!("{}: built a store, expected {expected}", row.name)
        }
        (None, None, Err(err)) => panic!("{}: {err}", row.name),
        (None, None, Ok(store)) => store,
    };

    assert_eq!(
        store.secret_store_id().map(str::to_string),
        row.id,
        "{}: id",
        row.name
    );
    assert_eq!(
        store.default_secret_store(),
        row.default,
        "{}: default",
        row.name
    );

    for lookup in &row.lookups {
        let answer = store.lookup(&lookup.name);
        match (&lookup.error, answer) {
            (Some(expected), Err(actual)) => {
                assert_eq!(
                    actual.to_string(),
                    *expected,
                    "{}: {}",
                    row.name,
                    lookup.why()
                );
            }
            (Some(expected), Ok(_)) => {
                panic!("{}: {}, expected {expected}", row.name, lookup.why());
            }
            (None, Err(err)) => panic!("{}: {}: {err}", row.name, lookup.why()),
            (None, Ok(actual)) => check_answer(row, lookup, actual),
        }
    }
}

/// Checks one answered lookup against the row.
fn check_answer(row: &StoreCase, lookup: &LookupAnswer, actual: Option<String>) {
    if lookup.from_process {
        // The document does not commit this machine's value: the process is the
        // oracle, and a missing variable means the store owes `None`.
        let expected = std::env::var(&lookup.name).ok();
        assert_eq!(
            actual,
            expected,
            "{}: {}: the store does not mirror the process environment",
            row.name,
            lookup.why()
        );
        assert_eq!(
            expected.is_some(),
            lookup.found,
            "{}: {}: this machine's environment disagrees with the row",
            row.name,
            lookup.why()
        );
        return;
    }
    assert_eq!(
        actual.as_deref(),
        lookup.found.then_some(lookup.value.as_str()),
        "{}: {}",
        row.name,
        lookup.why()
    );
}

/// Builds one store by implementation name, the way core's registry dispatch
/// does: decode the settings, then construct.
fn build(impl_name: &str, settings: &Value) -> flowcraft_secret::Result<Arc<dyn SecretStore>> {
    match impl_name {
        "env" => Ok(EnvSettings::decode(settings)?.build()),
        "file" => Ok(FileSettings::decode(settings)?.build()),
        other => panic!("the contract names a store this replay does not build: {other:?}"),
    }
}

/// Replaces every string equal to `$BASE`, at any depth, with `base`.
fn substitute_base(value: &Value, base: &Path) -> Value {
    const MARKER: &str = "$BASE";
    match value {
        Value::String(text) if text == MARKER => Value::String(base.to_string_lossy().into_owned()),
        Value::Array(items) => Value::Array(
            items
                .iter()
                .map(|item| substitute_base(item, base))
                .collect(),
        ),
        Value::Object(fields) => {
            let mut replaced = Map::new();
            for (key, item) in fields {
                replaced.insert(key.clone(), substitute_base(item, base));
            }
            Value::Object(replaced)
        }
        other => other.clone(),
    }
}

/// A directory that removes itself, since the crate has no `tempfile`.
struct TempDir {
    path: PathBuf,
}

impl TempDir {
    /// Creates an empty directory named after the row.
    fn new(name: &str) -> TempDir {
        let path =
            std::env::temp_dir().join(format!("flowcraft-secret-{name}-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&path);
        std::fs::create_dir_all(&path).unwrap_or_else(|err| panic!("{path:?}: {err}"));
        TempDir { path }
    }

    /// The directory, which the row's settings name as `$BASE`.
    fn path(&self) -> &Path {
        &self.path
    }

    /// Writes one file into the directory, creating parents as the oracle does.
    fn write(&self, file: &StoreFile) {
        let path = self.path.join(&file.name);
        if let Some(parent) = path.parent() {
            std::fs::create_dir_all(parent).unwrap_or_else(|err| panic!("{parent:?}: {err}"));
        }
        let data = if file.fill > 0 {
            vec![b'x'; file.fill as usize]
        } else {
            file.data.clone().into_bytes()
        };
        std::fs::write(&path, data).unwrap_or_else(|err| panic!("{path:?}: {err}"));
    }
}

impl Drop for TempDir {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.path);
    }
}

/// The committed document, compiled in so the test does not depend on the
/// working directory `cargo test` was invoked from.
const DOCUMENT: &str = include_str!("../../../conformance/secret-contract.json");
