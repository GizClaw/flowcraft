//! Regenerates the committed wire bindings under
//! `flowcraft/src/proto/generated/`.
//!
//! `proto/` at the repository root is the source of truth, shared with the Go
//! SDK. The generated Rust is committed so that building `flowcraft` — and
//! publishing it — needs neither `protoc` nor a build script: a consumer
//! compiles the bindings that are already in the package.
//!
//! This is a repository tool, not an example of using the library, and it is
//! `publish = false`: it reads `proto/` from the repository root, which is not
//! inside the published package. (It was an example target first; cargo always
//! ships the files a target needs, so the published crate carried a program
//! pointing at a directory that is not there.)
//!
//! `make rust-proto-gen` runs it, and `make rust-proto-check` runs it and fails
//! on drift, the same way `make proto-check` guards the Go stubs.
//!
//! ```bash
//! cargo run -p flowcraft-proto-gen   # needs protoc on PATH
//! ```
//!
//! Adding `proto/flowcraft/<area>/v1/*.proto` needs no change here: the tree is
//! walked, and every package becomes one module named after its area
//! (`flowcraft.workspace.v1` → `crate::proto::workspace`); a non-`v1` package
//! appends its version (`flowcraft.foo.v2` → `foo_v2`) so two versions can be
//! served side by side during a migration.

// Exempt from the async/blocking gate (`rust/clippy.toml`): a synchronous
// repository tool with no executor to starve — the first of the three documented
// cases.
#![allow(clippy::disallowed_methods)]

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

/// The conversation model: a data schema referenced by craft envelopes and tool
/// results. It is compiled on its own so every other package can name it
/// through `extern_path`, which keeps one copy of the types in the module tree.
const MESSAGE_PACKAGE: &str = ".flowcraft.message.v1";

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let manifest = PathBuf::from(std::env::var("CARGO_MANIFEST_DIR")?);
    // `rust/tools/proto-gen` → the repository root, which owns `proto/` and the
    // crate the bindings belong to.
    let root = manifest.join("../../..");
    let proto_root = root.join("proto");
    let out = root.join("rust/flowcraft/src/proto/generated");

    let mut files = Vec::new();
    collect(&proto_root, &mut files)?;
    files.sort();
    if files.is_empty() {
        return Err(format!("no protos under {}", proto_root.display()).into());
    }

    // Package → module name, in one deterministic pass.
    let mut modules: BTreeMap<String, String> = BTreeMap::new();
    for file in &files {
        let package = package_of(file)
            .ok_or_else(|| format!("{}: no package declaration", file.display()))?;
        let key = package.trim_start_matches('.').to_owned();
        let parts: Vec<&str> = key.split('.').collect();
        if parts.len() != 3 || parts[0] != "flowcraft" {
            return Err(format!("{}: unsupported package {package:?}", file.display()).into());
        }
        let module = if parts[2] == "v1" {
            parts[1].to_owned()
        } else {
            format!("{}_{}", parts[1], parts[2])
        };
        if modules.contains_key(&key) {
            return Err(format!("duplicate proto package {package:?}").into());
        }
        modules.insert(key, module);
    }

    // `extern_path` marks a package as externally provided, which also stops
    // prost from generating that package's file — so the message module is
    // compiled on its own first, and everything that references it is compiled
    // with the mapping that matches this crate's module layout.
    let (message_files, other_files): (Vec<PathBuf>, Vec<PathBuf>) = files
        .iter()
        .cloned()
        .partition(|file| package_of(file).as_deref() == Some(MESSAGE_PACKAGE));

    // prost writes `<package>.rs` next to its siblings in a directory it owns;
    // the scratch copy is renamed into the module names below, so no stale file
    // of a removed package survives a regeneration.
    let scratch = std::env::temp_dir().join(format!("flowcraft-proto-gen-{}", std::process::id()));
    if scratch.exists() {
        std::fs::remove_dir_all(&scratch)?;
    }
    std::fs::create_dir_all(&scratch)?;

    tonic_prost_build::configure()
        .build_client(true)
        .build_server(true)
        .emit_rerun_if_changed(false)
        .out_dir(&scratch)
        .compile_protos(&message_files, std::slice::from_ref(&proto_root))?;

    tonic_prost_build::configure()
        .build_client(true)
        .build_server(true)
        .emit_rerun_if_changed(false)
        .out_dir(&scratch)
        .extern_path(MESSAGE_PACKAGE, "crate::proto::message")
        .compile_protos(&other_files, std::slice::from_ref(&proto_root))?;

    if out.exists() {
        std::fs::remove_dir_all(&out)?;
    }
    std::fs::create_dir_all(&out)?;

    for (package, module) in &modules {
        let from = scratch.join(format!("{package}.rs"));
        let to = out.join(format!("{module}.rs"));
        std::fs::rename(&from, &to).map_err(|error| format!("{}: {error}", from.display()))?;
    }
    std::fs::remove_dir_all(&scratch)?;

    std::fs::write(out.join("mod.rs"), module_list(&modules))?;

    let shown = std::fs::canonicalize(&out).unwrap_or_else(|_| out.clone());
    println!(
        "generated {} bindings into {}",
        modules.len(),
        shown.display()
    );
    Ok(())
}

/// The module wrappers, one per proto package.
///
/// `include!` is deliberate: rustfmt only walks `mod` declarations, so the
/// generated bodies are never reformatted, and `cargo fmt --check` stays
/// meaningful for handwritten code. The allowances are the crate-level lints
/// the workspace turns up (`missing_docs`, `unwrap_used`, …) which generated
/// code cannot satisfy.
fn module_list(modules: &BTreeMap<String, String>) -> String {
    // The file is `include!`d from `src/proto/mod.rs`, so it is not a module
    // file of its own: rustfmt never rewrites it, and the lint policy for the
    // generated tree lives in the one handwritten file beside it.
    let mut text = String::from(
        "// @generated by tools/proto-gen — do not edit.\n\
         // One module per package; the module documentation is in the parent.\n\n",
    );
    for module in modules.values() {
        text.push_str(&format!(
            "pub mod {module} {{\n    include!(\"{module}.rs\");\n}}\n\n"
        ));
    }
    // Exactly one trailing newline: the file is regenerated on demand, and a
    // stray blank line is noise in the drift check.
    text.truncate(text.trim_end().len() + 1);
    text
}

/// The package name of a `.proto` file, with a leading dot as prost expects.
fn package_of(file: &Path) -> Option<String> {
    let text = std::fs::read_to_string(file).ok()?;
    text.lines()
        .find_map(|line| line.trim().strip_prefix("package "))
        .and_then(|rest| rest.strip_suffix(';'))
        .map(|name| format!(".{}", name.trim()))
}

fn collect(root: &Path, out: &mut Vec<PathBuf>) -> std::io::Result<()> {
    for entry in std::fs::read_dir(root)? {
        let entry = entry?;
        let path = entry.path();
        if path.is_dir() {
            collect(&path, out)?;
        } else if path.extension().is_some_and(|ext| ext == "proto") {
            out.push(path);
        }
    }
    Ok(())
}
