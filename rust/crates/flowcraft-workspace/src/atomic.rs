//! [`atomic_write`] and [`copy`]: the two composed operations every caller
//! needs, written once against the trait.
//!
//! `atomic_write` is the pattern in core's interface documentation made
//! executable: write to a temp path in the *same* directory, then
//! [`Workspace::rename`] over the live path, so a concurrent reader sees either
//! the old payload or the new one and never a half-written file. The temp file
//! is placed next to the target because a cross-directory rename is not atomic
//! even on POSIX.
//!
//! The temp name is an implementation detail with one observable consequence: it
//! can appear in a [`Workspace::list`] result for as long as the write is in
//! flight, so a consumer that enumerates a directory tolerates entries ending in
//! `.tmp`. Its shape -- `.<base>.tmp` beside the target -- is in the contract
//! because the failure path names it, and a rename that fails after a successful
//! write must still be able to clean up.
//!
//! # The failure path
//!
//! When `rename` fails, the temp file is deleted before the error is returned.
//! That cleanup is best-effort: the rename failure is the error that matters, and
//! a leftover `.tmp` is a disk leak rather than a lost write. core logs the
//! cleanup failure through telemetry; the port has no sink yet (telemetry is its
//! own slice) and swallows it, which is visible in the contract as "the temp file
//! is gone and the rename error is what came back".

use flowcraft_contract::error::Error;

use crate::clean::{base, dir, join};
use crate::{Result, Workspace};

/// Publishes `data` at `path` atomically where the medium allows it.
pub fn atomic_write(ws: &dyn Workspace, path: &str, data: &[u8]) -> Result<()> {
    let tmp = join(&[&dir(path), &format!(".{}.tmp", base(path))]);

    if let Err(err) = ws.write(&tmp, data) {
        return Err(wrap(format!("workspace atomicwrite: write tmp {tmp}"), err));
    }
    match ws.rename(&tmp, path) {
        Ok(()) => Ok(()),
        Err(err) => {
            // Best effort: the rename failure is the error worth surfacing.
            let _ = ws.delete(&tmp);
            Err(wrap(
                format!("workspace atomicwrite: rename {tmp} -> {path}"),
                err,
            ))
        }
    }
}

/// Copies one file to another path within the same workspace.
///
/// Reads the whole payload, so this is for files already known to be small; a
/// caller with a cap uses [`crate::capped`] first.
pub fn copy(ws: &dyn Workspace, src: &str, dst: &str) -> Result<()> {
    let data = ws
        .read(src)
        .map_err(|err| wrap(format!("workspace copy: read {src}"), err))?;
    ws.write(dst, &data)
        .map_err(|err| wrap(format!("workspace copy: write {dst}"), err))
}

/// Prepends `prefix` to `err`'s sentence, keeping its classification and cause.
///
/// This is core's `fmt.Errorf("%s: %w", prefix, err)`: the text grows a prefix,
/// the classification stays the innermost one that exists, and the cause remains
/// reachable.
fn wrap(prefix: String, err: Error) -> Error {
    let kind = err.kind();
    let message = format!("{prefix}: {err}");
    Error::with_source(kind, message, Box::new(err))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mem::MemWorkspace;
    use crate::testing::{ok, refusal, refused};

    #[test]
    fn the_temp_file_is_beside_the_target_and_gone_afterwards() {
        let ws = MemWorkspace::new();
        ok(atomic_write(&ws, "dir/a.txt", b"new"));
        assert_eq!(ws.paths(), vec!["dir/a.txt"]);
        assert_eq!(ok(ws.read("dir/a.txt")), b"new");
    }

    #[test]
    fn a_root_level_target_gets_no_leading_separator() {
        let ws = MemWorkspace::with_files([("a.txt", "old")]);
        ok(atomic_write(&ws, "a.txt", b"new"));
        assert_eq!(ws.paths(), vec!["a.txt"]);
    }

    #[test]
    fn a_failed_rename_cleans_up_and_names_both_paths() {
        let ws = MemWorkspace::with_files([("a.txt", "old")]);
        ws.refuse_rename(true);
        let err = refusal(atomic_write(&ws, "a.txt", b"new"));
        assert_eq!(
            err.to_string(),
            "workspace atomicwrite: rename .a.txt.tmp -> a.txt: fixture: rename refused"
        );
        // The temp file was removed and the live file kept the old payload.
        assert_eq!(ws.paths(), vec!["a.txt"]);
        assert_eq!(ok(ws.read("a.txt")), b"old");
    }

    #[test]
    fn copy_wraps_read_and_write_separately() {
        let ws = MemWorkspace::with_files([("a.txt", "x")]);
        ok(copy(&ws, "a.txt", "b/c.txt"));
        assert_eq!(ok(ws.read("b/c.txt")), b"x");
        assert_eq!(
            refused(copy(&ws, "nope.txt", "c.txt")),
            "workspace copy: read nope.txt: workspace: not found: nope.txt"
        );
    }
}
