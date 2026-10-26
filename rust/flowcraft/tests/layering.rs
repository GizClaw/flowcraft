#![allow(clippy::unwrap_used, clippy::expect_used)]

//! Pins the module graph documented in `rust/README.md`.
//!
//! The layers used to be crates, so Cargo enforced the one-way edges for free
//! and this test could read `cargo metadata`. They are modules of one crate now —
//! deliberately, because the crate is the published unit — so the edges have to
//! be checked the other way: scan what each module reaches for with `crate::`
//! and compare it to the table below.
//!
//! It is an approximation, and the shape of the approximation is the point:
//! it reads *code paths*, not prose. A documentation link such as
//! `crate::kernel::identity` in a `//!` comment is not an edge and is stripped
//! before the scan; a `use crate::kernel::…` in `resource` is, and fails.
//!
//! Two things about the whole package are checked as well, because they cannot
//! be expressed by the table: there are exactly two packages, and the daemon is
//! the only thing allowed to depend on the library.

use std::collections::{BTreeMap, BTreeSet};
use std::path::{Path, PathBuf};
use std::process::Command;

/// The one-way edges, self excluded. `logerr` is the crate-root macro the
/// taxonomy-shaped layers report failures through; it is not a layer.
const LAYERS: &[(&str, &[&str])] = &[
    // The crate root and the capability namespace only declare modules; a
    // `crate::` path there would mean a layer moved into the wrong place.
    ("root", &[]),
    ("caps", &[]),
    ("error", &[]),
    ("telemetry", &["error"]),
    ("proto", &[]),
    ("message", &["error", "proto"]),
    ("search", &["error"]),
    ("resource", &["error", "logerr"]),
    ("kernel", &["error", "telemetry", "resource", "logerr"]),
    ("client", &["error", "telemetry", "proto", "kernel"]),
    (
        "caps::workspace",
        &["error", "resource", "kernel", "client", "proto"],
    ),
    (
        "caps::secret",
        &["error", "resource", "kernel", "client", "proto"],
    ),
    (
        "caps::sandbox",
        &["error", "resource", "kernel", "client", "proto"],
    ),
    (
        "caps::tool",
        &["error", "resource", "kernel", "client", "proto", "search"],
    ),
];

#[test]
fn the_module_graph_is_one_way() {
    let table: BTreeMap<&str, BTreeSet<&str>> = LAYERS
        .iter()
        .map(|(module, allowed)| (*module, allowed.iter().copied().collect()))
        .collect();

    let mut walked: BTreeSet<String> = BTreeSet::new();
    let mut violations: Vec<String> = Vec::new();

    for (file, owner) in sources() {
        walked.insert(owner.clone());
        let allowed = table
            .get(owner.as_str())
            .unwrap_or_else(|| panic!("{owner} ({}) is not in the layer table", file.display()));
        let text = strip_comments(&read(&file));
        for referenced in crate_roots(&text) {
            if referenced == owner || allowed.contains(referenced.as_str()) {
                continue;
            }
            violations.push(format!(
                "{}: `{}` reaches `crate::{}`, which is not a layer below it",
                file.strip_prefix(root()).unwrap_or(&file).display(),
                owner,
                referenced
            ));
        }
    }

    assert!(
        violations.is_empty(),
        "the module graph is no longer one-way:\n{}",
        violations.join("\n")
    );

    // A rename that misses the table would otherwise disable its own check.
    let missing: Vec<&str> = table
        .keys()
        .copied()
        .filter(|module| !walked.contains(*module))
        .collect();
    assert!(
        missing.is_empty(),
        "layer table names {missing:?}, source does not"
    );
}

#[test]
fn the_daemon_is_the_only_dependent_of_the_library() {
    let value = metadata();
    let packages = value["packages"].as_array().expect("packages");
    let mut names: Vec<&str> = packages
        .iter()
        .map(|p| p["name"].as_str().expect("package name"))
        .collect();
    names.sort();
    assert_eq!(
        names,
        ["flowcraft", "flowcraft-platformd", "flowcraft-proto-gen"],
        "a new workspace member needs a deliberate decision, not an accident"
    );

    // The published surface is what `cargo publish` can reach: everything else
    // must say `publish = false`, or it becomes a third crate on crates.io.
    let mut published: Vec<&str> = Vec::new();
    for package in packages {
        let name = package["name"].as_str().expect("package name");
        let unpublished = package["publish"]
            .as_array()
            .is_some_and(|targets| targets.is_empty());
        if !unpublished {
            published.push(name);
        }
        for dep in package["dependencies"].as_array().expect("dependencies") {
            // `kind` is null for a normal dependency; "dev"/"build" otherwise.
            if !dep["kind"].is_null() {
                continue;
            }
            let dep_name = dep["name"].as_str().expect("dependency name");
            assert!(
                dep_name != "flowcraft" || name == "flowcraft-platformd",
                "{name} depends on the library; only the daemon may"
            );
            assert!(
                !dep_name.starts_with("flowcraft-") || dep_name == "flowcraft-platformd",
                "the split crates are gone; {name} still names {dep_name}"
            );
        }
    }
    published.sort_unstable();
    assert_eq!(
        published,
        ["flowcraft", "flowcraft-platformd"],
        "only two packages may be publishable"
    );
}

/// Every `.rs` file under `src/`, with the layer it belongs to.
///
/// `src/bin` is skipped: a binary target is its own crate root, so `crate::`
/// there means the binary, not the library.
fn sources() -> Vec<(PathBuf, String)> {
    let mut files = Vec::new();
    collect(&root().join("src"), &mut files);
    files
        .into_iter()
        .filter_map(|file| {
            let relative = file.strip_prefix(root().join("src")).ok()?.to_path_buf();
            let parts: Vec<&str> = relative.iter().filter_map(|p| p.to_str()).collect();
            let owner = match parts.first().copied()? {
                "bin" => return None,
                "lib.rs" => "root".to_owned(),
                "caps" => match parts.get(1) {
                    Some(cap) if parts.len() > 2 => format!("caps::{cap}"),
                    _ => "caps".to_owned(),
                },
                other => other.to_owned(),
            };
            Some((file, owner))
        })
        .collect()
}

fn collect(dir: &Path, out: &mut Vec<PathBuf>) {
    for entry in std::fs::read_dir(dir).expect("readable source directory") {
        let path = entry.expect("directory entry").path();
        if path.is_dir() {
            collect(&path, out);
        } else if path.extension().is_some_and(|ext| ext == "rs") {
            out.push(path);
        }
    }
}

fn root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
}

fn read(path: &Path) -> String {
    std::fs::read_to_string(path).expect("readable source file")
}

/// Drops `//` and `/* */` comments, keeping string literals intact.
///
/// Prose is not an edge. Several module docs name a layer to explain what they
/// do *not* depend on (`telemetry` says it renders identity "which is
/// `crate::kernel::identity`"), and counting those would make the gate
/// unfixable without editing documentation.
fn strip_comments(text: &str) -> String {
    let bytes: Vec<char> = text.chars().collect();
    let mut out = String::with_capacity(text.len());
    let mut index = 0;
    let mut block_depth = 0;

    while index < bytes.len() {
        let here = bytes[index];
        let next = bytes.get(index + 1).copied().unwrap_or('\0');

        if block_depth > 0 {
            if here == '/' && next == '*' {
                block_depth += 1;
                index += 2;
                continue;
            }
            if here == '*' && next == '/' {
                block_depth -= 1;
                index += 2;
                continue;
            }
            index += 1;
            continue;
        }

        if here == '/' && next == '*' {
            block_depth = 1;
            index += 2;
            continue;
        }

        if here == '/' && next == '/' {
            // A line comment runs to the newline; keep the newline so line
            // numbers in a failure message stay honest.
            while index < bytes.len() && bytes[index] != '\n' {
                index += 1;
            }
            continue;
        }

        if here == '"' {
            // Copy the literal verbatim, including escapes.
            out.push(here);
            index += 1;
            while index < bytes.len() {
                let c = bytes[index];
                out.push(c);
                index += 1;
                if c == '\\' {
                    if let Some(escaped) = bytes.get(index) {
                        out.push(*escaped);
                        index += 1;
                    }
                    continue;
                }
                if c == '"' {
                    break;
                }
            }
            continue;
        }

        out.push(here);
        index += 1;
    }
    out
}

/// The layers a file names through `crate::`, as `scope::name` or `name`.
fn crate_roots(text: &str) -> BTreeSet<String> {
    let mut found = BTreeSet::new();
    for (offset, _) in text.match_indices("crate::") {
        let rest = &text[offset + "crate::".len()..];
        let Some(first) = identifier(rest) else {
            continue;
        };
        if first != "caps" {
            found.insert(first);
            continue;
        }
        // `crate::caps::<cap>` is the layer; `crate::caps` alone is its root.
        let after = &rest[first.len()..];
        let second = after.strip_prefix("::").and_then(identifier);
        match second {
            Some(cap) => {
                found.insert(format!("caps::{cap}"));
            }
            None => {
                found.insert("caps".to_owned());
            }
        }
    }
    found
}

fn identifier(text: &str) -> Option<String> {
    let mut chars = text.char_indices().peekable();
    let (_, first) = *chars.peek()?;
    if !(first.is_ascii_alphabetic() || first == '_') {
        return None;
    }
    let mut end = 0;
    for (index, c) in chars {
        if c.is_ascii_alphanumeric() || c == '_' {
            end = index + c.len_utf8();
        } else {
            break;
        }
    }
    Some(text[..end].to_owned())
}

fn metadata() -> serde_json::Value {
    let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".to_owned());
    let output = Command::new(cargo)
        .args(["metadata", "--no-deps", "--format-version", "1"])
        .current_dir(root())
        .output()
        .expect("cargo metadata runs");
    assert!(
        output.status.success(),
        "cargo metadata failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    serde_json::from_slice(&output.stdout).expect("metadata json")
}
