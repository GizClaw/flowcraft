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
//! The backends that were measured and rejected are behind the non-default
//! `candidates` feature; the manifest says why.

mod gojson;
mod saphyr_backends;
#[cfg(feature = "candidates")]
mod serde_backends;

use std::env;
use std::fs;
use std::path::{Path, PathBuf};

use serde_json::json;

/// Mirrors `utils.FormatOf`: JSON when the first non-whitespace byte is `{`.
pub fn is_json_prefix(data: &[u8]) -> bool {
    data.iter()
        .find(|b| !b.is_ascii_whitespace())
        .is_some_and(|b| *b == b'{')
}

/// JSON input is validated and passed through as-is, like `utils.ToJSON`.
pub fn json_passthrough(data: &[u8]) -> Result<String, String> {
    let text = std::str::from_utf8(data).map_err(|e| format!("invalid UTF-8: {e}"))?;
    serde_json::from_str::<serde_json::Value>(text)
        .map_err(|e| format!("invalid JSON document: {e}"))?;
    Ok(text.to_string())
}

type Backend = (&'static str, fn(&[u8]) -> Result<String, String>);

const BACKENDS: &[Backend] = &[
    #[cfg(feature = "candidates")]
    ("norway", serde_backends::norway),
    #[cfg(feature = "candidates")]
    ("ng", serde_backends::yaml_ng),
    ("saphyr", saphyr_backends::naive),
    ("saphyr-go", saphyr_backends::go_layer),
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
        let format = if is_json_prefix(&data) {
            "json"
        } else {
            "yaml"
        };
        for (backend, run) in BACKENDS {
            let record = match run(&data) {
                Ok(json) => json!({
                    "file": file, "backend": backend, "format": format, "json": json,
                }),
                Err(error) => json!({
                    "file": file, "backend": backend, "format": format, "error": error,
                }),
            };
            println!("{record}");
        }
    }
}
