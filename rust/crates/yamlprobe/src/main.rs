//! YAML-stack probe: runs every candidate decoder over a file corpus and
//! prints one JSON record per (file, backend) pair.
//!
//! Records: {"file":..., "backend":..., "format":"json"|"yaml",
//!           "json":...} on success, or {..., "error":...} on failure.
//!
//! The Go side's `utils.ToJSON` output for the same files is the reference
//! (tools/conformance-oracle); see rust/conformance/tools/diff.py and the
//! gate that runs both: rust/conformance/run.sh.
//!
//! `saphyr-go` is not a reading of its own: it is `flowcraft-utils`, the code
//! the port ships, so the gate compares the port against the oracle through
//! this binary. The other three are measurements, kept runnable so the choice
//! between them stays reproducible rather than remembered. The two that were
//! rejected are behind the non-default `candidates` feature; the manifest says
//! why.

mod naive;
#[cfg(feature = "candidates")]
mod serde_backends;

use std::env;
use std::fs;
use std::path::{Path, PathBuf};

use flowcraft_utils::decode::{self, Format};
use serde_json::json;

type Backend = (&'static str, fn(&[u8]) -> Result<String, String>);

const BACKENDS: &[Backend] = &[
    #[cfg(feature = "candidates")]
    ("norway", serde_backends::norway),
    #[cfg(feature = "candidates")]
    ("ng", serde_backends::yaml_ng),
    ("saphyr", naive::naive),
    ("saphyr-go", decode::to_json),
];

fn walk(dir: &Path, out: &mut Vec<PathBuf>) {
    let Ok(entries) = fs::read_dir(dir) else {
        return;
    };
    for entry in entries.flatten() {
        let path = entry.path();
        let name = entry.file_name();
        let name = name.to_string_lossy();
        if path.is_dir() {
            if matches!(name.as_ref(), "target" | "node_modules" | ".git") {
                continue;
            }
            walk(&path, out);
        } else if path.is_file() {
            out.push(path);
        }
    }
}

fn main() {
    let roots: Vec<String> = env::args().skip(1).collect();
    if roots.is_empty() {
        eprintln!("usage: yamlprobe <path> [path...]");
        std::process::exit(2);
    }
    let mut files = Vec::new();
    for root in &roots {
        let path = Path::new(root);
        if path.is_dir() {
            walk(path, &mut files);
        } else if path.is_file() {
            files.push(path.to_path_buf());
        }
    }
    files.sort();

    for path in files {
        let ext = path
            .extension()
            .and_then(|e| e.to_str())
            .unwrap_or_default()
            .to_ascii_lowercase();
        if !matches!(ext.as_str(), "yaml" | "yml" | "json") {
            continue;
        }
        let data = match fs::read(&path) {
            Ok(data) => data,
            Err(err) => {
                eprintln!("read {}: {err}", path.display());
                continue;
            }
        };
        let file = path.to_string_lossy().to_string();
        let format = decode::format_of(&data);
        for (backend, run) in BACKENDS {
            // The JSON branch is core's own, not a reader's: `utils.ToJSON`
            // validates the bytes and returns them unchanged, so every reading
            // of a JSON document gives the same answer and no backend is
            // asked. The backends below only ever see YAML.
            let result = if format == Format::Json {
                decode::to_json(&data)
            } else {
                run(&data)
            };
            let record = match result {
                Ok(json) => json!({
                    "file": file, "backend": backend, "format": format.as_str(), "json": json,
                }),
                Err(error) => json!({
                    "file": file, "backend": backend, "format": format.as_str(), "error": error,
                }),
            };
            println!("{record}");
        }
    }
}
