//! Replays the committed hooks contract against the port.
//!
//! One row is one `hooks.json` -- plus the sources beside it -- and the events
//! fired at it. The harness reproduces each row the way the dump did: it makes a
//! directory of its own, changes into it (a primary source's commands run in the
//! process's directory, not a source's), writes the row's files, loads them, fires
//! what the row says, and reads back what the commands appended.
//!
//! What is compared is the *answer*, not the transcript: the load's refusal or
//! emptiness, the warnings it logged, and for every fire the commands that ran in
//! order with the JSON object each was handed. The four normalizations are the
//! dump's, restated in [`mask`] and [`mask_warning`], and none of them is this
//! file's invention:
//!
//!   - the harness's directory is masked out of every string;
//!   - a matcher's compile error and a JSON parse error are masked after the
//!     sentence that classifies them, because `regexp`/`regex` and
//!     `encoding/json`/`serde_json` word their own errors differently;
//!   - a warning's `error` attribute is masked for the same reason.
//!
//! The row that outlives its timeout records whether the process group died, by
//! looking for a file a descendant would have written. That is a clock in a
//! contract, and it is the one row whose comparison costs a second and a half.

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use flowcraft_hooks::{ExtraSource, Manager, Payload, Warn, Warning, event_names};
use serde_json::{Map, Value, json};

const CONTRACT: &str = include_str!("../../../conformance/hooks-contract.json");

/// Where every recorder command appends, relative to the harness's directory.
const TRACE: &str = "trace.part";

/// The file a timed-out row's descendant would write if it survived the kill.
const LATE: &str = "late.part";

/// How long past the hook's one-second timeout the harness waits before deciding
/// the descendant is gone.
const LATE_GRACE: Duration = Duration::from_millis(1400);

#[tokio::test]
async fn the_hooks_contract_replays() {
    let document = contract();
    let rows = rows(&document);
    assert!(
        rows.len() >= 21,
        "the contract has {} rows, expected at least 21: a table that shrank would replay fewer claims",
        rows.len()
    );

    // The vocabulary is core's, and the document records it in canonical order,
    // because that order is what an unknown-event message lists.
    let events: Vec<String> = document
        .get("events")
        .and_then(Value::as_array)
        .unwrap_or_else(|| panic!("the document has no events"))
        .iter()
        .map(|event| event.as_str().unwrap_or_default().to_owned())
        .collect();
    assert_eq!(
        events,
        event_names(),
        "the port's event vocabulary is not core's"
    );

    let directory = harness_dir();
    // The primary source's commands run in the process's directory, which is how
    // core's dump made them write next to the extras'. Every other test in this
    // crate runs in its own binary, so nothing else is reading this.
    let previous =
        std::env::current_dir().unwrap_or_else(|err| panic!("the process has no directory: {err}"));
    std::env::set_current_dir(&directory)
        .unwrap_or_else(|err| panic!("the harness cannot enter its directory: {err}"));

    let mut problems = Vec::new();
    for row in rows {
        replay(row, &directory, &mut problems).await;
    }

    std::env::set_current_dir(&previous)
        .unwrap_or_else(|err| panic!("the harness cannot leave its directory: {err}"));
    let _ = std::fs::remove_dir_all(&directory);
    report(problems);
}

// ---------------------------------------------------------------- one row

/// Replay one row into `problems`, as a set of readable mismatches.
async fn replay(row: &Value, directory: &Path, problems: &mut Vec<String>) {
    let name = string(row, "name");
    if let Err(err) = fire_row(row, directory, problems).await {
        problems.push(format!("{name}: the row could not run: {err}"));
    }
}

/// Materialize, load, fire, and compare. The `Err` arm is the harness failing,
/// not the port: a row that could not be set up says so rather than reporting a
/// mismatch it never measured.
async fn fire_row(
    row: &Value,
    directory: &Path,
    problems: &mut Vec<String>,
) -> std::io::Result<()> {
    let name = string(row, "name");
    clear(directory, TRACE)?;

    let config = row
        .get("config")
        .and_then(Value::as_str)
        .unwrap_or_default();
    let primary = if config.is_empty() {
        // A row with no config is a deployment with no hooks.json: the file it
        // names must not exist, so the harness names one nothing wrote.
        directory.join("absent.json")
    } else {
        directory.join("hooks.json")
    };
    if !config.is_empty() {
        std::fs::write(&primary, config)?;
    }

    let mut sources = Vec::new();
    for extra in row
        .get("extras")
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .unwrap_or_default()
    {
        let path = directory.join(string(extra, "path"));
        std::fs::write(&path, string(extra, "config"))?;
        let dir = match string(extra, "dir").as_str() {
            "." => directory.to_path_buf(),
            other => directory.join(other),
        };
        sources.push(ExtraSource {
            path,
            dir,
            trusted: extra
                .get("trusted")
                .and_then(Value::as_bool)
                .unwrap_or(false),
        });
    }

    let warnings = Arc::new(Sink::default());
    let load = Manager::load_with_sources(&primary, &sources, &warnings);
    compare(
        row,
        "load_warnings",
        &warnings.take(directory),
        problems,
        &name,
    );
    let manager = match load {
        Ok(manager) => manager,
        Err(err) => {
            compare_str(
                row,
                "error",
                &mask(&err.to_string(), directory),
                problems,
                &name,
            );
            return Ok(());
        }
    };
    compare(row, "empty", &Value::Bool(manager.empty()), problems, &name);

    for (index, fire) in row
        .get("fires")
        .and_then(Value::as_array)
        .map(Vec::as_slice)
        .unwrap_or_default()
        .iter()
        .enumerate()
    {
        clear(directory, TRACE)?;
        warnings.take(directory);
        let event = string(fire, "event");
        manager.fire(&event, &payload(fire), &warnings).await;
        let observed = json!({
            "trace": trace(directory)?,
            "warnings": warnings.take(directory),
        });
        compare(
            fire,
            "trace",
            &observed["trace"],
            problems,
            &format!("{name}: fires[{index}]"),
        );
        compare(
            fire,
            "warnings",
            &observed["warnings"],
            problems,
            &format!("{name}: fires[{index}]"),
        );
    }

    if let Some(killed) = row.get("descendant_killed").and_then(Value::as_bool) {
        // The hook's own timeout was one second; this is the wait that lets a
        // survivor prove it, or a kill prove itself.
        tokio::time::sleep(LATE_GRACE).await;
        let survived = directory.join(LATE).exists();
        if killed == survived {
            problems.push(format!(
                "{name}: core's descendant {}, the port's {}: {} is {}",
                if killed { "was killed" } else { "survived" },
                if survived { "survived" } else { "was killed" },
                LATE,
                if survived { "present" } else { "absent" },
            ));
        }
    }
    Ok(())
}

// --------------------------------------------------------------- the mask

/// The dump's masks, applied to one string: the harness's directory, then a
/// matcher's or a JSON document's own error text.
fn mask(text: &str, directory: &Path) -> String {
    let directory = directory.to_string_lossy().into_owned();
    let text = text.replace(&format!("{directory}/"), "");
    let text = text.replace(&directory, ".");
    if let Some(at) = text.find(": matcher: ") {
        return format!("{}: matcher: <regexp>", &text[..at]);
    }
    // `hooks: parse <path>: <error>` -- matched without a leading colon because
    // both places it appears start a message rather than follow one: a primary
    // file's refusal and a skipped source's warning.
    if let Some(at) = text.find("hooks: parse ") {
        let prefix = &text[..at + "hooks: parse ".len()];
        let rest = &text[prefix.len()..];
        if let Some(colon) = rest.find(": ") {
            return format!("{prefix}{}: <parse error>", &rest[..colon]);
        }
    }
    text
}

/// The warnings recorded since the last take, masked: a warning's `error`
/// attribute is masked whole, and every other field gets [`mask`] because an
/// operator's log line carries the command they wrote.
fn mask_warnings(warnings: &[Warning], directory: &Path) -> Value {
    Value::Array(
        warnings
            .iter()
            .map(|warning| {
                let mut fields = Map::new();
                for (key, value) in &warning.fields {
                    let value = if *key == "error" {
                        "<error>".to_owned()
                    } else {
                        mask(value, directory)
                    };
                    fields.insert((*key).to_owned(), Value::String(value));
                }
                let mut record = Map::new();
                record.insert(
                    "message".to_owned(),
                    Value::String(warning.message.to_owned()),
                );
                if !fields.is_empty() {
                    record.insert("fields".to_owned(), Value::Object(fields));
                }
                Value::Object(record)
            })
            .collect(),
    )
}

/// Collects the runner's warnings, the way core's log exporter did.
#[derive(Default)]
struct Sink {
    records: Mutex<Vec<Warning>>,
}

impl Sink {
    fn take(&self, directory: &Path) -> Value {
        let mut records = self.records.lock().unwrap_or_else(|err| err.into_inner());
        let masked = mask_warnings(&records, directory);
        records.clear();
        masked
    }
}

impl Warn for Sink {
    fn warn(&self, warning: &Warning) {
        self.records
            .lock()
            .unwrap_or_else(|err| err.into_inner())
            .push(warning.clone());
    }
}

// --------------------------------------------------------------- the trace

/// Every command that ran, in order, with the body it received. An absent file is
/// an empty list: the document says "no hook ran" that way.
fn trace(directory: &Path) -> std::io::Result<Value> {
    let text = match std::fs::read_to_string(directory.join(TRACE)) {
        Ok(text) => text,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => String::new(),
        Err(err) => return Err(err),
    };
    let mut calls = Vec::new();
    for line in text.lines().filter(|line| !line.is_empty()) {
        let at = match line.find(':') {
            Some(at) => at,
            None => {
                return Err(std::io::Error::new(
                    std::io::ErrorKind::InvalidData,
                    format!("the trace line has no label: {line:?}"),
                ));
            }
        };
        let body: Value = serde_json::from_str(&line[at + 1..]).map_err(|err| {
            std::io::Error::new(
                std::io::ErrorKind::InvalidData,
                format!("the trace line has no body: {line:?}: {err}"),
            )
        })?;
        calls.push(json!({ "label": &line[..at], "body": body }));
    }
    Ok(Value::Array(calls))
}

/// A row's payload as the runner takes it: strings only, which is what the dump
/// records (every field core documents for an event is a string).
fn payload(fire: &Value) -> Payload {
    fire.get("payload")
        .and_then(Value::as_object)
        .cloned()
        .unwrap_or_default()
}

// ------------------------------------------------------------ the harness

/// A directory the harness owns, removed when the replay finishes.
fn harness_dir() -> PathBuf {
    let path =
        std::env::temp_dir().join(format!("flowcraft-hooks-contract-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&path);
    std::fs::create_dir_all(&path).unwrap_or_else(|err| panic!("{path:?}: {err}"));
    path
}

/// Remove a trace file between fires, so one fire's trace is one fire's.
fn clear(directory: &Path, name: &str) -> std::io::Result<()> {
    match std::fs::remove_file(directory.join(name)) {
        Ok(()) => Ok(()),
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(err) => Err(err),
    }
}

// ---------------------------------------------------------- the comparison

/// Compare one observed field against the committed row's, pushing a readable
/// mismatch when they differ. A key the committed row omits is compared against
/// empty-or-false, since that is what the dump's `omitempty` means.
fn compare(row: &Value, field: &str, observed: &Value, problems: &mut Vec<String>, where_: &str) {
    let want = row.get(field);
    let same = match (want, observed) {
        (None, Value::Array(items)) => items.is_empty(),
        (None, Value::Bool(false)) => true,
        (None, Value::Object(fields)) => fields.is_empty(),
        (Some(want), observed) => want == observed,
        (None, observed) => observed.is_null(),
    };
    if same {
        return;
    }
    problems.push(format!(
        "{where_}: {field} differs\n  core: {}\n  port: {observed}",
        want.map(Value::to_string)
            .unwrap_or_else(|| "absent".to_owned())
    ));
}

/// [`compare`] for a string, so a refusal reads as its sentence rather than as a
/// JSON literal.
fn compare_str(row: &Value, field: &str, observed: &str, problems: &mut Vec<String>, where_: &str) {
    let want = row.get(field).and_then(Value::as_str);
    if want == Some(observed) {
        return;
    }
    problems.push(format!(
        "{where_}: {field} differs\n  core: {}\n  port: {observed:?}",
        want.map(|want| format!("{want:?}"))
            .unwrap_or_else(|| "absent".to_owned())
    ));
}

fn report(problems: Vec<String>) {
    if problems.is_empty() {
        return;
    }
    panic!(
        "{} of the committed hooks rows do not replay:\n{}",
        problems.len(),
        problems.join("\n")
    );
}

fn contract() -> Value {
    match serde_json::from_str(CONTRACT) {
        Ok(document) => document,
        Err(err) => panic!("the committed hooks contract does not parse: {err}"),
    }
}

fn rows(document: &Value) -> &[Value] {
    match document.get("rows").and_then(Value::as_array) {
        Some(rows) => rows,
        None => panic!("the document has no rows table"),
    }
}

fn string(row: &Value, field: &str) -> String {
    row.get(field)
        .and_then(Value::as_str)
        .unwrap_or_else(|| panic!("a row has no {field}: {row}"))
        .to_owned()
}
