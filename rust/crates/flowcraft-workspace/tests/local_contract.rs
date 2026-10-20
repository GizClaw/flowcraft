//! The local backend, replayed: the same questions the map medium answers, put to
//! a real directory tree where a path can leave.
//!
//! What makes this section different from `workspace_contract.rs` is in the
//! document: each row is built in two areas (`root`, the workspace, and
//! `outside`, a sibling the symlinks reach for), and each error carries a `kind`.
//! When that kind is `internal`, the host wrote the sentence -- core's `os.Root`
//! says `path escapes from parent`, `cap-std` says `a path led outside of the
//! filesystem` -- so the row asserts the outcome and the classification and not the
//! words. Every other kind is one of core's own refusals, and its sentence is
//! compared exactly.
//!
//! The trees are temporary, and no row names one: a symlink fixture of
//! `abs:<path>` is made absolute against the workspace root by each side, so the
//! document is portable even though the medium is not.

#![cfg(unix)]

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use flowcraft_contract::error::{Error, ErrorKind};
use flowcraft_workspace::{
    LocalWorkspace, Workspace, atomic_write, capped, copy, glob_detailed, walk,
};
use serde::{Deserialize, Serialize};

/// The committed document, of which this test reads the `local` half.
#[derive(Deserialize)]
struct Contract {
    /// The real-tree section.
    local: Local,
}

/// The real-filesystem half of the table.
#[derive(Deserialize)]
struct Local {
    /// The trees and their answers.
    cases: Vec<Case>,
}

/// One temporary tree and the questions put to it.
#[derive(Debug, Deserialize, Serialize)]
struct Case {
    /// Identifies the case in a mismatch.
    name: String,
    /// `raw` or `sub`; empty means `raw`.
    #[serde(default)]
    kind: String,
    /// The `sub` prefixes, outermost last.
    #[serde(default)]
    prefixes: Vec<String>,
    /// The fixtures, in the workspace or beside it.
    #[serde(default)]
    files: Vec<File>,
    /// The refusal a `sub` answered with instead of a view.
    #[serde(default)]
    build: String,
    /// That refusal's classification.
    #[serde(default)]
    build_kind: String,
    /// The questions, in order.
    #[serde(default)]
    ops: Vec<Op>,
    /// The workspace tree after the operations.
    #[serde(rename = "final")]
    final_files: Vec<Entry>,
    /// The tree beside the workspace.
    #[serde(default)]
    final_outside: Vec<File>,
}

/// One fixture.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
struct File {
    /// Where it is written.
    path: String,
    /// Its bytes; absent for a symlink.
    #[serde(default)]
    data: String,
    /// Its symlink target, when it is a link.
    #[serde(default)]
    symlink: String,
    /// `outside`, for a fixture beside the workspace.
    #[serde(default)]
    area: String,
}

/// One operation and its answer.
#[derive(Debug, Default, Deserialize, Serialize)]
struct Op {
    /// The operation's name.
    op: String,
    /// Its subject.
    #[serde(default)]
    path: String,
    /// A second path, or the bytes to write.
    #[serde(default)]
    arg: String,
    /// The read cap.
    #[serde(default)]
    max: i64,
    /// Whether it returned no error.
    ok: bool,
    /// A read's contents or a stat's summary.
    #[serde(default)]
    data: String,
    /// `exists`' answer.
    #[serde(rename = "bool", default)]
    bool_answer: bool,
    /// A list's, walk's or glob's answer.
    #[serde(default)]
    list: Vec<Entry>,
    /// The refusal's text.
    #[serde(default)]
    error: String,
    /// The refusal's classification; `internal` means the words are the host's.
    #[serde(default)]
    kind: String,
}

/// One name or path in an answer.
#[derive(Debug, Clone, Deserialize, Serialize, PartialEq, Eq)]
struct Entry {
    /// The entry's name (from `list`) or path (from `walk` and `glob`).
    path: String,
    /// Whether it is a directory.
    #[serde(default)]
    dir: bool,
}

#[test]
fn the_local_contract_replays() {
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
    assert!(
        !contract.local.cases.is_empty(),
        "the local section has cases"
    );

    for (index, case) in contract.local.cases.iter().enumerate() {
        replay(index, case);
    }
}

/// Runs one case and compares every answer.
fn replay(index: usize, expected: &Case) {
    let base = scratch(index, &expected.name);
    let root = base.join("root");
    let outside = base.join("outside");
    std::fs::create_dir_all(&root).unwrap_or_else(|err| panic!("{}: root: {err}", expected.name));
    std::fs::create_dir_all(&outside)
        .unwrap_or_else(|err| panic!("{}: outside: {err}", expected.name));
    for fixture in &expected.files {
        apply(&root, &outside, fixture)
            .unwrap_or_else(|err| panic!("{}: fixture {}: {err}", expected.name, fixture.path));
    }

    let workspace = LocalWorkspace::new(&root)
        .unwrap_or_else(|err| panic!("{}: open the workspace: {err}", expected.name));

    let mut view: Box<dyn Workspace> = Box::new(workspace.clone());
    let mut build: Option<Error> = None;
    for prefix in &expected.prefixes {
        match workspace.sub(prefix) {
            Ok(sub) => view = Box::new(sub),
            Err(err) => {
                build = Some(err);
                break;
            }
        }
    }
    match (&build, expected.build.is_empty()) {
        (None, false) => panic!(
            "case {}: the view was built, the row expected a refusal",
            expected.name
        ),
        (Some(err), true) => panic!("case {}: unexpected refusal {err}", expected.name),
        (Some(err), false) => {
            assert_eq!(
                err.to_string(),
                expected.build,
                "case {}: the refusal a prefix gets",
                expected.name
            );
            assert_eq!(
                kind_of(Some(err)),
                expected.build_kind,
                "case {}: that refusal's classification",
                expected.name
            );
        }
        (None, true) => {}
    }

    let mut actual = Vec::new();
    for question in &expected.ops {
        actual.push(run_op(view.as_ref(), question));
    }
    assert_eq!(
        actual.len(),
        expected.ops.len(),
        "case {}: the number of operations moved",
        expected.name
    );
    for (index, (want, got)) in expected.ops.iter().zip(actual.iter()).enumerate() {
        let where_in_case = format!(
            "case {}: operation {index} (`{}` on `{}` of `{}`)",
            expected.name, want.op, want.path, want.arg
        );
        assert_eq!(got.ok, want.ok, "{where_in_case}: the outcome");
        assert_eq!(got.kind, want.kind, "{where_in_case}: the classification");
        if want.kind != INTERNAL {
            assert_eq!(got.error, want.error, "{where_in_case}: the sentence");
        }
        assert_eq!(got.data, want.data, "{where_in_case}: the data");
        assert_eq!(
            got.bool_answer, want.bool_answer,
            "{where_in_case}: the boolean"
        );
        assert_eq!(got.list, want.list, "{where_in_case}: the list");
    }

    assert_eq!(
        tree(&root),
        expected.final_files,
        "case {}: the workspace tree after the operations",
        expected.name
    );
    assert_eq!(
        files(&outside),
        expected.final_outside,
        "case {}: the tree beside the workspace",
        expected.name
    );

    let _ = std::fs::remove_dir_all(&base);
}

/// The marker for a sentence the host wrote.
const INTERNAL: &str = "internal";

/// A temporary base directory, unique to the run and the case.
fn scratch(index: usize, name: &str) -> PathBuf {
    let mut base = std::env::temp_dir();
    base.push(format!(
        "flowcraft-replay-{index}-{name}-{}",
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&base);
    base
}

/// Writes one fixture into its area.
fn apply(root: &Path, outside: &Path, fixture: &File) -> std::io::Result<()> {
    let area = if fixture.area == "outside" {
        outside
    } else {
        root
    };
    let target = area.join(&fixture.path);
    if let Some(parent) = target.parent() {
        std::fs::create_dir_all(parent)?;
    }
    if fixture.symlink.is_empty() {
        return std::fs::write(&target, fixture.data.as_bytes());
    }
    let link = match fixture.symlink.strip_prefix("abs:") {
        Some(rest) => root.join(rest),
        None => PathBuf::from(&fixture.symlink),
    };
    std::os::unix::fs::symlink(link, target)
}

/// The workspace tree after the operations, directories included.
fn tree(root: &Path) -> Vec<Entry> {
    let mut entries = Vec::new();
    collect(root, root, &mut entries);
    entries
}

/// Appends one directory's children, in name order, depth first.
fn collect(root: &Path, dir: &Path, entries: &mut Vec<Entry>) {
    let read = match std::fs::read_dir(dir) {
        Ok(read) => read,
        Err(err) => panic!("the tree is not readable at {}: {err}", dir.display()),
    };
    let mut children: Vec<PathBuf> = read
        .map(|child| match child {
            Ok(child) => child.path(),
            Err(err) => panic!("a directory entry is not readable: {err}"),
        })
        .collect();
    children.sort();
    for child in children {
        let relative = match child.strip_prefix(root) {
            Ok(relative) => relative.to_string_lossy().replace('\\', "/"),
            Err(_) => panic!("a child is not under the root: {}", child.display()),
        };
        let file_type = match std::fs::symlink_metadata(&child) {
            Ok(meta) => meta.file_type(),
            Err(err) => panic!("{}: {err}", child.display()),
        };
        entries.push(Entry {
            path: relative,
            dir: file_type.is_dir(),
        });
        if file_type.is_dir() {
            collect(root, &child, entries);
        }
    }
}

/// The regular files beside the workspace, with their bytes.
fn files(outside: &Path) -> Vec<File> {
    let mut out = Vec::new();
    let mut names = BTreeMap::new();
    collect_files(outside, outside, &mut names);
    for (path, data) in names {
        out.push(File {
            path,
            data,
            symlink: String::new(),
            area: String::new(),
        });
    }
    out
}

/// Collects a tree's files into `names`.
fn collect_files(root: &Path, dir: &Path, names: &mut BTreeMap<String, String>) {
    let read = match std::fs::read_dir(dir) {
        Ok(read) => read,
        Err(err) => panic!("{}: {err}", dir.display()),
    };
    for child in read {
        let child = match child {
            Ok(child) => child.path(),
            Err(err) => panic!("a directory entry is not readable: {err}"),
        };
        let meta = match std::fs::symlink_metadata(&child) {
            Ok(meta) => meta,
            Err(err) => panic!("{}: {err}", child.display()),
        };
        if meta.is_dir() {
            collect_files(root, &child, names);
            continue;
        }
        if !meta.file_type().is_file() {
            continue;
        }
        let data = match std::fs::read(&child) {
            Ok(data) => data,
            Err(err) => panic!("{}: {err}", child.display()),
        };
        let path = child
            .strip_prefix(root)
            .unwrap_or(&child)
            .to_string_lossy()
            .replace('\\', "/");
        let text = match String::from_utf8(data) {
            Ok(text) => text,
            Err(err) => panic!("a fixture stopped being text: {err}"),
        };
        names.insert(path, text);
    }
}

/// Performs one operation and records its answer.
fn run_op(view: &dyn Workspace, question: &Op) -> Op {
    let mut answer = Op {
        op: question.op.clone(),
        path: question.path.clone(),
        arg: question.arg.clone(),
        max: question.max,
        ..Op::default()
    };

    let outcome: Result<(), Error> = match question.op.as_str() {
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
        "capabilities" => {
            let caps = view.capabilities();
            answer.data = format!(
                "{},{},{},{}",
                caps.atomic_rename, caps.read_after_write, caps.durable_on_write, caps.distributed
            );
            Ok(())
        }
        other => panic!("the contract asks for an operation the replay does not know: {other}"),
    };

    let failure = outcome.err();
    if let Some(err) = &failure
        && answer.error.is_empty()
    {
        answer.error = err.to_string();
    }
    answer.ok = answer.error.is_empty();
    answer.kind = kind_of(failure.as_ref());
    answer
}

/// Classifies an error the way the document does.
fn kind_of(err: Option<&Error>) -> String {
    let Some(err) = err else {
        return String::new();
    };
    match err.kind() {
        ErrorKind::NotFound => "not_found",
        ErrorKind::Validation => "validation",
        ErrorKind::Forbidden => "forbidden",
        ErrorKind::NotAvailable => "not_available",
        ErrorKind::Conflict => "conflict",
        ErrorKind::Timeout => "timeout",
        _ => INTERNAL,
    }
    .to_owned()
}

/// The fixture bytes as text.
fn text_of(data: Vec<u8>) -> String {
    match String::from_utf8(data) {
        Ok(text) => text,
        Err(err) => panic!("a fixture stopped being text: {err}"),
    }
}
