//! Composed operations every provider inherits.
//!
//! These are the parts of the contract that are *policy over the primitive*, not
//! storage: publishing a payload atomically, copying, bounded reads, and the two
//! traversals (`walk`, `glob`). They are free functions over `&dyn Workspace` so
//! a provider does not have to reimplement them and a decorator gets them for
//! free.
//!
//! Paths in and out are the portable workspace namespace: `/`-separated on every
//! platform, relative to the workspace root, with no leading `./`. The Go side
//! makes the same promise for `Walk`/`Glob`, and it matters for callers that
//! compare a returned path against a prefix.

use std::future::Future;
use std::pin::Pin;

use flowcraft_error::{Error, ErrorKind, Result};

use crate::contract::{Entry, Workspace};

/// What a [`walk`] visitor asks for next.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Step {
    /// Keep going: descend when the entry is a directory.
    Continue,
    /// Skip the rest of this entry. Skips a subtree for a directory and is a
    /// no-op for a file.
    SkipDir,
}

/// Read `path` under a byte cap.
///
/// A provider that can refuse at the read layer is asked first (see
/// [`Workspace::read_limited`]), because that is the only shape in which an
/// oversized file never lands in memory. A provider that cannot is read whole
/// and checked afterwards: correct, but the fallback is a fallback, not the
/// rule.
///
/// # Errors
///
/// `max_bytes` must be positive ([`ErrorKind::Validation`]); the oversized case
/// is [`ErrorKind::Validation`] too, matching the Go side's classification.
pub async fn capped(workspace: &dyn Workspace, path: &str, max_bytes: u64) -> Result<Vec<u8>> {
    if max_bytes == 0 {
        return Err(Error::validation(
            "workspace: read cap must be positive, got 0",
        ));
    }
    let data = match workspace.read_limited(path, max_bytes).await {
        Ok(data) => data,
        Err(error) if error.kind() == ErrorKind::NotAvailable => workspace.read(path).await?,
        Err(error) => return Err(error),
    };
    // Defensive: a backend that answers the bounded-read contract but ignores
    // the cap must not reach a decoder either.
    if data.len() as u64 > max_bytes {
        return Err(Error::validation(format!(
            "workspace: {path} is {} bytes, over the {max_bytes}-byte read cap",
            data.len()
        )));
    }
    Ok(data)
}

/// Publish `data` at `path` so a reader never observes a half-written payload.
///
/// The temp file sits in the destination directory, so the rename stays atomic
/// on a medium that can rename atomically (`Capabilities::atomic_rename`). The
/// temp name is a private detail but it can appear in a concurrent `list`, so
/// consumers that enumerate a directory should tolerate entries that end in
/// `.tmp`.
///
/// # Errors
///
/// The write or the rename failure is reported; a leftover temp file after a
/// failed rename is cleaned up best-effort and logged.
pub async fn atomic_write(workspace: &dyn Workspace, path: &str, data: &[u8]) -> Result<()> {
    let tmp = temp_path(path);
    workspace.write(&tmp, data).await.map_err(|error| {
        context(
            "atomic_write",
            format!("write tmp {tmp}"),
            Error::new(error.kind(), error.message()),
        )
    })?;
    match workspace.rename(&tmp, path).await {
        Ok(()) => Ok(()),
        Err(error) => {
            if let Err(cleanup) = workspace.delete(&tmp).await {
                tracing::warn!(
                    path = %tmp,
                    cleanup = %cleanup,
                    "workspace: removing the temp file after a failed atomic write failed"
                );
            }
            Err(context(
                "atomic_write",
                format!("rename {tmp} -> {path}"),
                error,
            ))
        }
    }
}

/// Copy one file within the workspace.
///
/// # Errors
///
/// The read or the write failure is reported, with the operation that failed.
pub async fn copy(workspace: &dyn Workspace, src: &str, dst: &str) -> Result<()> {
    let data = workspace
        .read(src)
        .await
        .map_err(|error| context("copy", format!("read {src}"), error))?;
    workspace
        .write(dst, &data)
        .await
        .map_err(|error| context("copy", format!("write {dst}"), error))
}

/// Depth-first traversal: directories are visited before their contents.
///
/// `dir` is the subtree root; `"."` and `""` mean the workspace root. The
/// visitor receives portable `/`-separated paths and may return
/// [`Step::SkipDir`] to prune a subtree.
///
/// # Errors
///
/// The first listing or visitor error aborts the walk.
pub async fn walk<F>(workspace: &dyn Workspace, dir: &str, visit: &mut F) -> Result<()>
where
    F: FnMut(&str, &Entry) -> Result<Step> + Send,
{
    walk_dir(workspace, dir.to_owned(), visit).await
}

fn walk_dir<'a, F>(
    workspace: &'a dyn Workspace,
    dir: String,
    visit: &'a mut F,
) -> Pin<Box<dyn Future<Output = Result<()>> + Send + 'a>>
where
    F: FnMut(&str, &Entry) -> Result<Step> + Send,
{
    Box::pin(async move {
        for entry in workspace.list(&dir).await? {
            let child = join(&dir, &entry.name);
            if visit(&child, &entry)? == Step::SkipDir {
                continue;
            }
            if entry.is_dir {
                walk_dir(workspace, child, visit).await?;
            }
        }
        Ok(())
    })
}

/// Paths of the files matching a glob pattern, relative to the workspace root.
///
/// Patterns are matched against the whole portable path, with `*` and `?` kept
/// inside one path component and `**` spanning components: `*.json` matches only
/// top-level files, `**/*.json` matches at any depth. Patterns are
/// case-insensitive only where the host filesystem is (`cfg!(windows)`), the
/// same fold the Go side applies, so a policy cannot be bypassed by case.
///
/// # Errors
///
/// An invalid pattern is [`ErrorKind::Validation`], reported before any I/O.
pub async fn glob(workspace: &dyn Workspace, pattern: &str) -> Result<Vec<String>> {
    let matcher = crate::pattern::compile(pattern)?;
    let mut matches = Vec::new();
    walk(workspace, "", &mut |path, entry| {
        if !entry.is_dir && matcher.is_match(path) {
            matches.push(path.to_owned());
        }
        Ok(Step::Continue)
    })
    .await?;
    Ok(matches)
}

/// Join a workspace directory and a child name with the portable separator.
pub(crate) fn join(dir: &str, name: &str) -> String {
    if dir.is_empty() || dir == "." {
        name.to_owned()
    } else {
        format!("{dir}/{name}")
    }
}

/// The private temp path a payload is written to before it is published.
fn temp_path(path: &str) -> String {
    let (dir, base) = match path.rfind('/') {
        Some(index) => (&path[..index], &path[index + 1..]),
        None => ("", path),
    };
    if dir.is_empty() {
        format!(".{base}.tmp")
    } else {
        format!("{dir}/.{base}.tmp")
    }
}

fn context(operation: &str, what: String, error: Error) -> Error {
    Error::new(
        error.kind(),
        format!("workspace {operation}: {what}: {}", error.message()),
    )
    .with_source(error)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::local::LocalWorkspace;

    async fn workspace() -> (tempfile::TempDir, LocalWorkspace) {
        let dir = tempfile::tempdir().unwrap();
        let workspace = LocalWorkspace::open(dir.path().join("root")).unwrap();
        (dir, workspace)
    }

    #[tokio::test]
    async fn atomic_write_publishes_and_leaves_no_temp_file() {
        let (_dir, workspace) = workspace().await;
        atomic_write(&workspace, "a/b.txt", b"payload")
            .await
            .unwrap();
        assert_eq!(workspace.read("a/b.txt").await.unwrap(), b"payload");
        let names: Vec<String> = workspace
            .list("a")
            .await
            .unwrap()
            .into_iter()
            .map(|entry| entry.name)
            .collect();
        assert_eq!(names, vec!["b.txt".to_owned()]);
    }

    #[tokio::test]
    async fn copy_duplicates_a_file() {
        let (_dir, workspace) = workspace().await;
        workspace.write("src.txt", b"bytes").await.unwrap();
        copy(&workspace, "src.txt", "nested/dst.txt").await.unwrap();
        assert_eq!(workspace.read("nested/dst.txt").await.unwrap(), b"bytes");
    }

    #[tokio::test]
    async fn walk_is_depth_first_and_prunable() {
        let (_dir, workspace) = workspace().await;
        workspace.write("a/1.txt", b"1").await.unwrap();
        workspace.write("a/b/2.txt", b"2").await.unwrap();
        workspace.write("c.txt", b"3").await.unwrap();

        let mut seen = Vec::new();
        walk(&workspace, "", &mut |path, _entry| {
            seen.push(path.to_owned());
            Ok(Step::Continue)
        })
        .await
        .unwrap();
        assert_eq!(seen, vec!["a", "a/1.txt", "a/b", "a/b/2.txt", "c.txt"]);

        let mut pruned = Vec::new();
        walk(&workspace, "", &mut |path, entry| {
            if path == "a" && entry.is_dir {
                return Ok(Step::SkipDir);
            }
            pruned.push(path.to_owned());
            Ok(Step::Continue)
        })
        .await
        .unwrap();
        assert_eq!(pruned, vec!["c.txt"]);
    }

    #[tokio::test]
    async fn walk_on_a_missing_directory_is_empty() {
        let (_dir, workspace) = workspace().await;
        let mut seen = 0usize;
        walk(&workspace, "missing", &mut |_path, _entry| {
            seen += 1;
            Ok(Step::Continue)
        })
        .await
        .unwrap();
        assert_eq!(seen, 0);
    }

    #[tokio::test]
    async fn glob_keeps_a_star_inside_one_component() {
        let (_dir, workspace) = workspace().await;
        workspace.write("top.json", b"{}").await.unwrap();
        workspace.write("nested/deep.json", b"{}").await.unwrap();
        workspace.write("nested/deep.txt", b"").await.unwrap();

        assert_eq!(
            glob(&workspace, "*.json").await.unwrap(),
            vec!["top.json".to_owned()]
        );
        assert_eq!(
            glob(&workspace, "**/*.json").await.unwrap(),
            vec!["nested/deep.json".to_owned(), "top.json".to_owned()]
        );
    }

    #[tokio::test]
    async fn glob_rejects_an_invalid_pattern() {
        let (_dir, workspace) = workspace().await;
        let error = glob(&workspace, "a[b").await.unwrap_err();
        assert_eq!(error.kind(), ErrorKind::Validation);
    }

    /// A provider that refuses bounded reads, to exercise the fallback.
    struct PlainWorkspace;

    #[async_trait::async_trait]
    impl Workspace for PlainWorkspace {
        async fn capabilities(&self) -> Result<crate::contract::Capabilities> {
            Ok(crate::contract::Capabilities::default())
        }

        async fn read(&self, _path: &str) -> Result<Vec<u8>> {
            Ok(vec![0; 8])
        }

        async fn write(&self, _path: &str, _data: &[u8]) -> Result<()> {
            Ok(())
        }

        async fn append(&self, _path: &str, _data: &[u8]) -> Result<()> {
            Ok(())
        }

        async fn rename(&self, _from: &str, _to: &str) -> Result<()> {
            Ok(())
        }

        async fn delete(&self, _path: &str) -> Result<()> {
            Ok(())
        }

        async fn remove_all(&self, _path: &str) -> Result<()> {
            Ok(())
        }

        async fn stat(&self, _path: &str) -> Result<crate::contract::Stat> {
            Err(Error::not_available("plain"))
        }

        async fn exists(&self, _path: &str) -> Result<bool> {
            Ok(false)
        }

        async fn list(&self, _dir: &str) -> Result<Vec<crate::contract::Entry>> {
            Ok(Vec::new())
        }
    }

    #[tokio::test]
    async fn capped_falls_back_and_still_enforces_the_cap() {
        assert_eq!(capped(&PlainWorkspace, "file", 8).await.unwrap().len(), 8);
        let error = capped(&PlainWorkspace, "file", 7).await.unwrap_err();
        assert_eq!(error.kind(), ErrorKind::Validation);
        assert!(error.to_string().contains("read cap"));
    }

    #[tokio::test]
    async fn capped_prefers_the_bounded_read() {
        let (_dir, workspace) = workspace().await;
        workspace.write("file", b"0123456789").await.unwrap();
        assert_eq!(capped(&workspace, "file", 10).await.unwrap().len(), 10);
        assert_eq!(
            capped(&workspace, "file", 9).await.unwrap_err().kind(),
            ErrorKind::Validation
        );
    }
}
