//! File provider, in two shapes.
//!
//! `settings.base` is the deployment convention (docker secrets, mounted k8s
//! secrets): one file per secret, named by the secret name, read on every
//! lookup. A rotated file is picked up without a rebuild, escaping the base
//! directory is refused, symlinked secret files are refused, and one secret is
//! capped at [`MAX_BYTES`].
//!
//! `settings.path` keeps the earlier shape: one file holding either a JSON
//! object or `KEY=VALUE` lines, parsed once when the resource is built. That
//! read is bounded and auditable; a per-call read of a whole file would make the
//! file a live dependency of every request.
//!
//! The shape is chosen by which key the settings carry, and exactly one is
//! required.

use std::collections::BTreeMap;
use std::path::{Component, Path, PathBuf};
use std::sync::Arc;

use async_trait::async_trait;
use flowcraft_error::{Error, Result};
use flowcraft_resource::Kind;
use flowcraft_resource::{BoxFuture, BuildCtx, Factory, Phase, Spec};
use serde::Deserialize;
use serde_json::Value;

use crate::contract::{CONTRACT, Handle, KIND, Store};
use crate::memory::MemoryStore;

/// How large one secret file may be.
pub const MAX_BYTES: u64 = 1 << 20;

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct Settings {
    #[serde(default)]
    base: Option<String>,
    #[serde(default)]
    path: Option<String>,
    #[serde(default)]
    id: Option<String>,
    #[serde(default)]
    default: bool,
}

fn validate(value: &Value) -> Result<()> {
    let settings: Settings = flowcraft_resource::settings(value)?;
    match (&settings.base, &settings.path) {
        (Some(base), None) => {
            if base.trim().is_empty() {
                return Err(Error::validation("settings.base is required"));
            }
        }
        (None, Some(path)) => {
            if path.trim().is_empty() {
                return Err(Error::validation("settings.path is required"));
            }
        }
        (Some(_), Some(_)) => {
            return Err(Error::validation(
                "settings.base and settings.path are exclusive; use base for one file per secret",
            ));
        }
        (None, None) => {
            return Err(Error::validation(
                "settings.base (one file per secret) or settings.path (one file of pairs) is required",
            ));
        }
    }
    Ok(())
}

/// Parse either a JSON object of string values or `KEY=VALUE` lines.
fn parse(text: &str) -> Result<BTreeMap<String, String>> {
    if text.trim_start().starts_with('{') {
        return serde_json::from_str(text).map_err(|error| {
            Error::validation(format!("secret file is not a JSON object: {error}"))
        });
    }
    let mut values = BTreeMap::new();
    for (index, line) in text.lines().enumerate() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let Some((name, value)) = line.split_once('=') else {
            return Err(Error::validation(format!(
                "secret file line {}: expected KEY=VALUE",
                index + 1
            )));
        };
        let name = name.trim();
        if name.is_empty() {
            return Err(Error::validation(format!(
                "secret file line {}: name is empty",
                index + 1
            )));
        }
        values.insert(name.to_owned(), value.trim().to_owned());
    }
    Ok(values)
}

/// One file per secret under a base directory, read on every lookup.
#[derive(Debug)]
pub struct DirStore {
    root: PathBuf,
}

impl DirStore {
    /// Open the provider over `root`, which must exist.
    pub fn open(root: impl AsRef<Path>) -> Result<Self> {
        let root = root.as_ref();
        let metadata = std::fs::metadata(root).map_err(|error| {
            Error::validation(format!(
                "secret file: root {} is not usable: {error}",
                root.display()
            ))
        })?;
        if !metadata.is_dir() {
            return Err(Error::validation(format!(
                "secret file: root {} is not a directory",
                root.display()
            )));
        }
        let root = std::fs::canonicalize(root).map_err(|error| {
            Error::validation(format!(
                "secret file: resolve root {}: {error}",
                root.display()
            ))
        })?;
        Ok(Self { root })
    }

    /// The base directory.
    #[must_use]
    pub fn root(&self) -> &Path {
        &self.root
    }
}

#[async_trait]
impl Store for DirStore {
    async fn lookup(&self, name: &str) -> Result<Option<String>> {
        if name.trim().is_empty() {
            return Err(Error::validation("secret file: secret name is empty"));
        }
        if name != name.trim() {
            return Err(Error::validation(format!(
                "secret file: secret name {name:?} has surrounding whitespace"
            )));
        }
        let relative = Path::new(name);
        if relative.is_absolute()
            || relative
                .components()
                .any(|component| !matches!(component, Component::Normal(_)))
        {
            return Err(Error::validation(format!(
                "secret file: secret name {name:?} must be a relative path without '..'"
            )));
        }
        let path = self.root.join(relative);
        let metadata = match std::fs::symlink_metadata(&path) {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(error) => {
                return Err(Error::internal(format!(
                    "secret file: stat {name:?}: {error}"
                )));
            }
        };
        if metadata.file_type().is_symlink() {
            return Err(Error::validation(format!(
                "secret file: {name:?} is a symlink"
            )));
        }
        if !metadata.is_file() {
            return Err(Error::validation(format!(
                "secret file: {name:?} is not a regular file"
            )));
        }
        if metadata.len() > MAX_BYTES {
            return Err(Error::validation(format!(
                "secret file: {name:?} is {} bytes, over the {MAX_BYTES}-byte limit",
                metadata.len()
            )));
        }
        let real = std::fs::canonicalize(&path)
            .map_err(|error| Error::internal(format!("secret file: resolve {name:?}: {error}")))?;
        if !real.starts_with(&self.root) {
            return Err(Error::validation(format!(
                "secret file: {name:?} escapes the root"
            )));
        }
        let data = match std::fs::read(&real) {
            Ok(data) => data,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
            Err(error) => {
                return Err(Error::internal(format!(
                    "secret file: read {name:?}: {error}"
                )));
            }
        };
        if data.len() as u64 > MAX_BYTES {
            return Err(Error::validation(format!(
                "secret file: {name:?} is {} bytes, over the {MAX_BYTES}-byte limit",
                data.len()
            )));
        }
        let text = String::from_utf8(data)
            .map_err(|_| Error::internal(format!("secret file: {name:?} is not UTF-8")))?;
        // Mirror docker/k8s conventions: strip trailing newlines so an API key
        // written by `echo` or `printf` works.
        Ok(Some(text.trim_end_matches(['\r', '\n']).to_owned()))
    }
}

/// The `secret.Store/file` factory.
pub struct FileFactory;

impl Factory for FileFactory {
    type Output = Handle;

    fn spec(&self) -> Spec {
        let kind = Kind::new(KIND).unwrap_or_else(|error| panic!("{KIND} is invalid: {error}"));
        Spec {
            contract: Some(CONTRACT),
            imp: Some("file"),
            deps: Vec::new(),
            phase: Phase::Pre,
            validate_settings: Some(validate),
            ..Spec::new(kind)
        }
    }

    fn build<'a>(&'a self, ctx: BuildCtx<'a>) -> BoxFuture<'a, Result<Arc<Self::Output>>> {
        Box::pin(async move {
            let settings: Settings = ctx.settings()?;
            let store: Arc<dyn Store> = match (settings.base, settings.path) {
                (Some(base), None) => {
                    let mut root = PathBuf::from(&base);
                    if root.is_relative()
                        && let Some(dir) = ctx.base_dir
                    {
                        root = dir.join(root);
                    }
                    Arc::new(DirStore::open(root)?)
                }
                (None, Some(path)) => {
                    let mut path = PathBuf::from(&path);
                    if path.is_relative()
                        && let Some(base) = ctx.base_dir
                    {
                        path = base.join(path);
                    }
                    let text = tokio::fs::read_to_string(&path).await.map_err(|error| {
                        Error::not_found(format!("secret file {}: {error}", path.display()))
                    })?;
                    Arc::new(MemoryStore::new(parse(&text)?))
                }
                // `validate` refuses both cases before construction.
                _ => {
                    return Err(Error::validation(
                        "settings.base or settings.path is required",
                    ));
                }
            };
            Ok(Arc::new(Handle::with_settings(
                store,
                settings.id,
                settings.default,
            )))
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parses_json_objects() {
        let values = parse(r#"{"a": "1", "b": "2"}"#).expect("json");
        assert_eq!(values["a"], "1");
        assert_eq!(values.len(), 2);
    }

    #[test]
    fn parses_key_value_lines_with_comments() {
        let values = parse("# comment\n\nA=1\nB = two words \n").expect("lines");
        assert_eq!(values["A"], "1");
        assert_eq!(values["B"], "two words");
    }

    #[test]
    fn rejects_a_line_without_a_separator() {
        let error = parse("A=1\nnonsense\n").expect_err("bad line");
        assert!(error.to_string().contains("line 2"), "{error}");
    }

    #[test]
    fn rejects_an_empty_name() {
        assert!(parse("=1\n").is_err());
    }

    #[tokio::test]
    async fn serves_one_file_per_secret_and_strips_the_newline() {
        let dir = tempfile::tempdir().unwrap();
        std::fs::write(dir.path().join("token"), b"hunter2\n").unwrap();
        std::fs::create_dir_all(dir.path().join("team")).unwrap();
        std::fs::write(dir.path().join("team").join("key"), b"nested").unwrap();
        let store = DirStore::open(dir.path()).unwrap();
        assert_eq!(store.lookup("token").await.unwrap(), Some("hunter2".into()));
        assert_eq!(
            store.lookup("team/key").await.unwrap(),
            Some("nested".into())
        );
        assert_eq!(store.lookup("missing").await.unwrap(), None);
    }

    #[tokio::test]
    async fn refuses_escapes_and_symlinks() {
        let dir = tempfile::tempdir().unwrap();
        let outside = tempfile::tempdir().unwrap();
        std::fs::write(outside.path().join("secret"), b"nope").unwrap();
        let store = DirStore::open(dir.path()).unwrap();

        for name in ["../secret", "/etc/passwd", "team/../../secret", " ./x"] {
            assert!(store.lookup(name).await.is_err(), "name {name:?}");
        }

        #[cfg(unix)]
        {
            std::os::unix::fs::symlink(outside.path().join("secret"), dir.path().join("link"))
                .unwrap();
            let error = store.lookup("link").await.unwrap_err();
            assert!(error.to_string().contains("symlink"), "{error}");
        }
    }

    #[tokio::test]
    async fn refuses_an_oversized_secret() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("big");
        std::fs::write(&path, vec![b'x'; MAX_BYTES as usize + 1]).unwrap();
        let store = DirStore::open(dir.path()).unwrap();
        let error = store.lookup("big").await.unwrap_err();
        assert!(error.to_string().contains("over the"), "{error}");
    }

    #[test]
    fn the_base_directory_must_exist_and_be_a_directory() {
        let dir = tempfile::tempdir().unwrap();
        let error = DirStore::open(dir.path().join("nope")).unwrap_err();
        assert!(error.to_string().contains("not usable"), "{error}");
        let file = dir.path().join("file.txt");
        std::fs::write(&file, b"x").unwrap();
        let error = DirStore::open(&file).unwrap_err();
        assert!(error.to_string().contains("not a directory"), "{error}");
    }
}
