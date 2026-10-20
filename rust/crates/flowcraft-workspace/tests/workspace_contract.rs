//! The workspace contract: the rules core enforces around any backend, replayed.
//!
//! Nothing here is masked, and that is the point of the medium: every row runs
//! over a map of path to bytes (`flowcraft_workspace::mem`), not a temporary
//! directory, so no path in the document depends on the run. The Go oracle writes
//! the same map-shaped medium into its own package; the two must agree for a row
//! to pass.
//!
//! What a row asserts is what core's code *around* the medium answers: the two
//! sentences a bounded read can fail with, the temp name an atomic publish uses
//! and cleans up, the scope's two directions and the rule that refused, the
//! prefixed view's join and its own root refusal, and the order a walk visits a
//! tree in. `raw-ops` is the one row that asserts the medium, and it is there so
//! the others are not leaning on two harnesses that disagree.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use flowcraft_workspace::mem::MemWorkspace;
use flowcraft_workspace::{
    Scoped, ScopedOptions, ViolationLogger, ViolationRecord, Workspace, atomic_write, capped, copy,
    glob_detailed, sub, walk,
};
use serde::{Deserialize, Serialize};

/// The committed document.
#[derive(Deserialize)]
struct Contract {
    /// The fixtures and their answers.
    cases: Vec<Case>,
}

/// One medium, one view over it, and the questions put to the view.
#[derive(Debug, Deserialize, Serialize, PartialEq)]
struct Case {
    /// Identifies the case in a mismatch.
    name: String,
    /// What the case is evidence of.
    why: String,
    /// The medium's contents before the operations.
    files: Vec<File>,
    /// `raw`, `scope` or `sub`; empty means `raw`.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    kind: String,
    /// The `sub` prefixes, outermost last.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    prefixes: Vec<String>,
    /// The scope's read deny-list.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    deny_read: Vec<String>,
    /// The scope's write allow-list.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    allow_write: Vec<String>,
    /// Patterns refused in both directions.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    mandatory_deny: Vec<String>,
    /// Whether the medium answers bounded reads.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    limited: bool,
    /// Whether the medium fails every rename.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    refuse_rename: bool,
    /// The questions, in order.
    ops: Vec<Op>,
    /// The medium's contents after the operations.
    #[serde(rename = "final")]
    final_files: Vec<File>,
}

/// One entry in the medium.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
struct File {
    /// Where it is written, relative to the view.
    path: String,
    /// Its bytes.
    data: String,
}

/// One operation and its answer.
#[derive(Debug, Deserialize, Serialize, PartialEq, Eq)]
struct Op {
    /// The operation's name.
    op: String,
    /// Its subject, a glob's pattern, or a walk's root.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    path: String,
    /// A second path, the bytes to write, or the path a walk skips.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    arg: String,
    /// The read cap.
    #[serde(default, skip_serializing_if = "is_zero")]
    max: i64,
    /// Whether the operation returned no error.
    ok: bool,
    /// A read's contents, a stat's summary, or a glob's pattern.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    data: String,
    /// `exists`' answer.
    #[serde(rename = "bool", default, skip_serializing_if = "std::ops::Not::not")]
    bool_answer: bool,
    /// A list's, walk's or glob's answer.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    list: Vec<Entry>,
    /// The refusal's text.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    error: String,
    /// The records the scope logged for this operation.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    violations: Vec<Violation>,
}

/// One name or path in an answer.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
struct Entry {
    /// The entry's name (from `list`) or path (from `walk` and `glob`).
    path: String,
    /// Whether it is a directory.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    dir: bool,
}

/// One violation record, as the logger received it.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
struct Violation {
    /// The refused direction.
    operation: String,
    /// The path as the caller wrote it.
    path: String,
    /// The rule that refused.
    reason: String,
}

/// Serde needs a named predicate for `omitempty` on numbers.
fn is_zero(value: &i64) -> bool {
    *value == 0
}

/// Keeps every record a scope emits, in order.
#[derive(Default)]
struct Recorder(Mutex<Vec<ViolationRecord>>);

impl ViolationLogger for Recorder {
    fn log_violation(&self, record: &ViolationRecord) {
        match self.0.lock() {
            Ok(mut records) => records.push(record.clone()),
            Err(poisoned) => poisoned.into_inner().push(record.clone()),
        }
    }
}

impl Recorder {
    /// Takes the records accumulated since the last call.
    ///
    /// A poisoned lock is recovered: the recorder holds a `Vec` that a panicking
    /// case cannot have left torn, and losing the records would hide the reason a
    /// row failed.
    fn drain(&self) -> Vec<Violation> {
        let mut records = match self.0.lock() {
            Ok(records) => records,
            Err(poisoned) => poisoned.into_inner(),
        };
        let out = records
            .iter()
            .map(|record| Violation {
                operation: record.operation.clone(),
                path: record.path.clone(),
                reason: record.reason.clone(),
            })
            .collect();
        records.clear();
        out
    }
}

#[test]
fn the_workspace_contract_replays() {
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../../conformance/workspace-contract.json"
    );
    let text = match std::fs::read_to_string(path) {
        Ok(text) => text,
        Err(err) => panic!("the committed contract is not readable: {err}"),
    };
    let contract: Contract = match serde_json::from_str(&text) {
        Ok(contract) => contract,
        Err(err) => panic!("the committed contract does not decode: {err}"),
    };
    assert!(!contract.cases.is_empty(), "the contract has cases");

    for case in contract.cases {
        replay(&case);
    }
}

/// Runs one case and compares every answer.
fn replay(expected: &Case) {
    let medium = MemWorkspace::new();
    medium.refuse_rename(expected.refuse_rename);
    medium.sync_limited_reads(expected.limited);
    for file in &expected.files {
        medium
            .write(&file.path, file.data.as_bytes())
            .unwrap_or_else(|err| panic!("case {}: fixture {}: {err}", expected.name, file.path));
    }

    let recorder = Arc::new(Recorder::default());
    let view = view_over(&medium, expected, recorder.clone());

    let mut actual = Case {
        name: expected.name.clone(),
        why: expected.why.clone(),
        files: expected.files.clone(),
        kind: expected.kind.clone(),
        prefixes: expected.prefixes.clone(),
        deny_read: expected.deny_read.clone(),
        allow_write: expected.allow_write.clone(),
        mandatory_deny: expected.mandatory_deny.clone(),
        limited: expected.limited,
        refuse_rename: expected.refuse_rename,
        ops: Vec::new(),
        final_files: Vec::new(),
    };
    for question in &expected.ops {
        actual.ops.push(run_op(view.as_ref(), &recorder, question));
    }
    actual.final_files = snapshot(&medium);

    assert_eq!(
        actual.ops.len(),
        expected.ops.len(),
        "case {}: the number of operations moved",
        expected.name
    );
    for (index, (want, got)) in expected.ops.iter().zip(actual.ops.iter()).enumerate() {
        assert_eq!(
            got, want,
            "case {}: operation {index} (`{}` on `{}` of `{}`)",
            expected.name, want.op, want.path, want.arg
        );
    }
    assert_eq!(
        actual.final_files, expected.final_files,
        "case {}: the medium's contents after the operations",
        expected.name
    );
}

/// Builds the view a case asks for: raw, scoped, or one `sub` per prefix.
fn view_over(medium: &MemWorkspace, case: &Case, recorder: Arc<Recorder>) -> Box<dyn Workspace> {
    match case.kind.as_str() {
        "sub" => {
            let mut view: Box<dyn Workspace> = Box::new(medium.clone());
            for prefix in &case.prefixes {
                view = sub(view, prefix);
            }
            view
        }
        "scope" => {
            let options = ScopedOptions::new()
                .deny_read(case.deny_read.clone())
                .allow_write(case.allow_write.clone())
                .mandatory_deny(case.mandatory_deny.clone())
                .logger(recorder);
            Box::new(Scoped::new(Box::new(medium.clone()), options))
        }
        _ => Box::new(medium.clone()),
    }
}

/// Performs one operation and records its answer.
fn run_op(view: &dyn Workspace, recorder: &Arc<Recorder>, question: &Op) -> Op {
    let mut answer = Op {
        op: question.op.clone(),
        path: question.path.clone(),
        arg: question.arg.clone(),
        max: question.max,
        ok: false,
        data: String::new(),
        bool_answer: false,
        list: Vec::new(),
        error: String::new(),
        violations: Vec::new(),
    };

    let outcome: Result<(), flowcraft_contract::error::Error> = match question.op.as_str() {
        "read" => view.read(&question.path).map(|data| {
            answer.data = text_of(data);
        }),
        "write" => view.write(&question.path, question.arg.as_bytes()),
        "append" => view.append(&question.path, question.arg.as_bytes()),
        "rename" => view.rename(&question.path, &question.arg),
        "delete" => view.delete(&question.path),
        "remove_all" => view.remove_all(&question.path),
        "list" => view.list(&question.path).map(|entries| {
            answer.list = entries
                .into_iter()
                .map(|entry| Entry {
                    path: entry.name,
                    dir: entry.is_dir,
                })
                .collect();
        }),
        "exists" => view
            .exists(&question.path)
            .map(|yes| answer.bool_answer = yes),
        "stat" => view.stat(&question.path).map(|meta| {
            answer.data = format!("{}:{}:{}", meta.name, meta.len, meta.is_dir);
        }),
        "walk" => walk(view, &question.path, &mut |path, entry| {
            answer.list.push(Entry {
                path: path.to_string(),
                dir: entry.is_dir,
            });
            flowcraft_workspace::Step::Continue
        }),
        "walk_skip" => walk(view, &question.path, &mut |path, entry| {
            if path == question.arg && entry.is_dir {
                return flowcraft_workspace::Step::Skip;
            }
            answer.list.push(Entry {
                path: path.to_string(),
                dir: entry.is_dir,
            });
            flowcraft_workspace::Step::Continue
        }),
        "glob" => {
            answer.data = question.path.clone();
            glob_detailed(view, &question.path).map(|found| {
                answer.list = found
                    .matches
                    .into_iter()
                    .map(|path| Entry { path, dir: false })
                    .collect();
                answer.error = found.error.unwrap_or_default();
            })
        }
        "capped" => capped(view, &question.path, question.max).map(|data| {
            answer.data = text_of(data);
        }),
        "atomic" => atomic_write(view, &question.path, question.arg.as_bytes()),
        "copy" => copy(view, &question.path, &question.arg),
        other => panic!("the contract asks for an operation the replay does not know: {other}"),
    };

    if let Err(err) = outcome {
        // A glob's refusal is reported in `error` without an `Err` (core returns
        // the matches found before the bad segment), so `ok` is decided by the
        // sentence rather than by which arm set it.
        if answer.error.is_empty() {
            answer.error = err.to_string();
        }
    }
    answer.ok = answer.error.is_empty();
    answer.violations = recorder.drain();
    answer
}

/// The fixture bytes as text. Every byte in a row comes from a string in the
/// oracle's table, so a failure here would be a document this test cannot read.
fn text_of(data: Vec<u8>) -> String {
    match String::from_utf8(data) {
        Ok(text) => text,
        Err(err) => panic!("a fixture stopped being text: {err}"),
    }
}

/// The medium's contents, sorted by path.
fn snapshot(medium: &MemWorkspace) -> Vec<File> {
    let files: BTreeMap<String, Vec<u8>> = medium.snapshot();
    files
        .into_iter()
        .map(|(path, data)| File {
            path,
            data: text_of(data),
        })
        .collect()
}
