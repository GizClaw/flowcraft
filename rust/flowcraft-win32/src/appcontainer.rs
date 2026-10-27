//! AppContainer: the write boundary Windows enforces with a SID.
//!
//! An integrity level (see [`crate::token`]) is a *write-up* rule: a Low process
//! cannot modify Medium-labelled data, but it can still read everything the user
//! can read, and it can write to anything labelled Low. An AppContainer is a
//! different bargain: the process runs under a SID that owns nothing, so it can
//! only touch what an ACL explicitly grants that SID — reads included. That is the
//! shape the sandbox contract asks for with `write.read_only` and
//! `writable_paths`: a workspace that is writable, a rest of the filesystem that
//! is not even readable unless it was granted.
//!
//! The mechanism is short; the profiles are not:
//!
//! - a profile is a name plus the SID Windows derives from it, so the same name
//!   always resolves to the same SID, and the profile outlives the process
//!   (`CreateAppContainerProfile` once, `DeriveAppContainerSidFromAppContainerName`
//!   afterwards);
//! - the SID is what an ACL grants, which is why [`AppContainer::sid`] is public
//!   and the only thing the capability layer needs;
//! - a token derived from ours with that SID and **zero capabilities** can read
//!   what its ACLs allow and nothing else — no network, no user's files, no
//!   registry hive beyond its own.
//!
//! The SID does **not** travel on the token: `SetTokenInformation(TokenAppContainerSid)`
//! answers `ERROR_INVALID_PARAMETER`, because an AppContainer is applied when the
//! process is created (`PROC_THREAD_ATTRIBUTE_SECURITY_CAPABILITIES`), not by
//! rewriting a token afterwards. That attribute list is the next piece of W4; this
//! module is the profile and the SID it hands to it.

use std::ptr::null_mut;

use windows_sys::Win32::Foundation::ERROR_ALREADY_EXISTS;
use windows_sys::Win32::Security::Isolation::{
    CreateAppContainerProfile, DeleteAppContainerProfile, DeriveAppContainerSidFromAppContainerName,
};
use windows_sys::Win32::Security::PSID;

use crate::job::Error;

/// A profile's SID, owned by this value.
pub struct AppContainer {
    name: String,
    /// Allocated by Win32 (`CreateAppContainerProfile` or the derive call), freed
    /// with `FreeSid` in `Drop`. Held as a raw pointer because that is what every
    /// consumer takes, `SetEntriesInAcl` and `SetTokenInformation` alike.
    sid: PSID,
}

// SAFETY: a SID is an immutable byte blob with no thread affinity; the only
// mutation this crate does is user32/userenv writing into an ACL buffer, which
// never touches the SID itself. `Drop` frees it exactly once.
unsafe impl Send for AppContainer {}
// SAFETY: as above.
unsafe impl Sync for AppContainer {}

impl std::fmt::Debug for AppContainer {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("AppContainer")
            .field("name", &self.name)
            .finish_non_exhaustive()
    }
}

impl Drop for AppContainer {
    fn drop(&mut self) {
        if !self.sid.is_null() {
            // SAFETY: the SID came from one of the two Win32 calls that allocate
            // one, and neither was freed before this point.
            unsafe { windows_sys::Win32::Security::FreeSid(self.sid) };
        }
    }
}

impl AppContainer {
    /// Open the profile called `name`, creating it if this is the first time.
    ///
    /// Idempotent on purpose: a daemon restarts, and a profile that already
    /// exists is the normal case. Creating it twice is not an error either — the
    /// derive call is tried first, and the create call answers "already exists"
    /// by handing back the SID.
    pub fn open(name: &str) -> Result<Self, Error> {
        let wide = wide(name);
        let mut sid: PSID = null_mut();

        // SAFETY: `wide` is a NUL-terminated wide string that outlives the call,
        // and `sid` is a live out-pointer. The returned SID is owned by us.
        let hr = unsafe { DeriveAppContainerSidFromAppContainerName(wide.as_ptr(), &mut sid) };
        if hr >= 0 && !sid.is_null() {
            return Ok(Self {
                name: name.to_owned(),
                sid,
            });
        }

        let mut sid: PSID = null_mut();
        // SAFETY: three NUL-terminated wide strings and a live out-pointer; no
        // capabilities, which is the whole point — an AppContainer with none can
        // reach nothing it was not granted.
        let hr = unsafe {
            CreateAppContainerProfile(
                wide.as_ptr(),
                wide.as_ptr(),
                wide.as_ptr(),
                std::ptr::null(),
                0,
                &mut sid,
            )
        };
        if hr >= 0 {
            return Ok(Self {
                name: name.to_owned(),
                sid,
            });
        }
        // `HRESULT_FROM_WIN32(ERROR_ALREADY_EXISTS)` — the profile was created
        // between our two calls (two daemons, one machine). The derive call above
        // would succeed now, so treat it as success by asking once more.
        if hr as u32 == 0x8007_0000 | (ERROR_ALREADY_EXISTS & 0xffff) {
            let mut sid: PSID = null_mut();
            // SAFETY: as the first derive call.
            let hr = unsafe { DeriveAppContainerSidFromAppContainerName(wide.as_ptr(), &mut sid) };
            if hr >= 0 && !sid.is_null() {
                return Ok(Self {
                    name: name.to_owned(),
                    sid,
                });
            }
        }
        Err(Error::hresult(
            format!("sandbox: cannot create the AppContainer profile {name:?}"),
            hr,
        ))
    }

    /// The profile's name, as the document spelled it.
    #[must_use]
    pub fn name(&self) -> &str {
        &self.name
    }

    /// The SID an ACL (or a token) refers to.
    #[must_use]
    pub fn sid(&self) -> PSID {
        self.sid
    }

    /// Remove the profile. Called by tests and by a host that wants a clean
    /// machine; a session does not need it, because profiles are per-user state
    /// and cheap to reuse.
    pub fn delete(name: &str) -> Result<(), Error> {
        let wide = wide(name);
        // SAFETY: a NUL-terminated wide string; the call only removes per-user
        // profile state and touches no memory of ours.
        let hr = unsafe { DeleteAppContainerProfile(wide.as_ptr()) };
        if hr >= 0 {
            return Ok(());
        }
        Err(Error::hresult(
            format!("sandbox: cannot delete the AppContainer profile {name:?}"),
            hr,
        ))
    }
}

/// A NUL-terminated UTF-16 copy of `text`.
///
/// Interior NULs are rejected by truncation at the first one: a profile name with
/// an interior NUL is a caller bug, and silently handing Win32 half a name would
/// create a profile nobody asked for.
pub(crate) fn wide(text: &str) -> Vec<u16> {
    let mut wide: Vec<u16> = text.encode_utf16().collect();
    if let Some(nul) = wide.iter().position(|unit| *unit == 0) {
        wide.truncate(nul);
    }
    wide.push(0);
    wide
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A name unique to this process, so two runs and two tests cannot share a
    /// profile by accident.
    fn name(tag: &str) -> String {
        format!("flowcraft.test.{}.{}", tag, std::process::id())
    }

    #[test]
    fn a_profile_resolves_to_one_sid_every_time() {
        let name = name("sid");
        let first = AppContainer::open(&name).expect("create profile");
        let again = AppContainer::open(&name).expect("reopen profile");
        assert!(!first.sid().is_null());
        // The SID is derived from the name, so the second call — which takes the
        // derive path, not the create path — must land on the same value.
        assert_eq!(
            sid_string(first.sid()),
            sid_string(again.sid()),
            "the same name must derive the same SID"
        );
        AppContainer::delete(&name).expect("delete profile");
    }

    /// Run one `cmd /c` script, optionally inside a container, and answer its
    /// exit code.
    fn run(container: Option<&AppContainer>, script: &str) -> i32 {
        let comspec =
            std::env::var("COMSPEC").unwrap_or_else(|_| r"C:\Windows\System32\cmd.exe".to_owned());
        let args = vec!["/c".to_owned(), script.to_owned()];
        let mut spawned = crate::process::spawn(crate::Launch {
            command: crate::Command {
                program: &comspec,
                args: &args,
                cwd: None,
                env: None,
            },
            job: None,
            token: None,
            container,
        })
        .expect("spawn");
        // Our ends of the three pipes must go, or the child blocks writing.
        drop(spawned.take_stdin());
        drop(spawned.take_stdout());
        drop(spawned.take_stderr());
        spawned.into_child().wait().expect("wait") as i32
    }

    /// The property W4 exists for: a contained child owns nothing it was not
    /// granted, so a write to the daemon's own temp directory is refused.
    ///
    /// The control is the same command without a container — if that failed too,
    /// the test would be proving something about `cmd`, not about the boundary.
    #[test]
    fn a_contained_child_cannot_write_outside_the_container() {
        let name = name("write");
        let container = AppContainer::open(&name).expect("create profile");
        let dir = std::env::temp_dir().join(format!("flowcraft.ac.{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("temp dir");
        let probe = dir.join("probe.txt");
        // `cmd` owns the redirection, so the script contains a `>` — and the
        // argument builder quotes anything with a space, which is what makes
        // `cmd /c "echo … > C:\path"` behave. The path itself must not contain a
        // space: inside the script it cannot be quoted again without nesting
        // quotes that `cmd` then reads literally (which is how this test first
        // failed, at the control).
        assert!(
            !probe.display().to_string().contains(' '),
            "the probe path must not contain a space: {}",
            probe.display()
        );
        let script = format!("echo hello > {}", probe.display());

        let _ = std::fs::remove_file(&probe);
        let plain = run(None, &script);
        assert_eq!(plain, 0, "the control must be able to write");
        assert!(probe.exists(), "the control must have written the file");

        std::fs::remove_file(&probe).expect("remove probe");
        let contained = run(Some(&container), &script);
        assert_ne!(
            contained, 0,
            "a contained child must not write to a path its SID was not granted"
        );
        assert!(
            !probe.exists(),
            "the contained child must not have created the file"
        );

        let _ = std::fs::remove_dir_all(&dir);
        AppContainer::delete(&name).expect("delete profile");
    }

    /// A SID as `S-1-15-2-…`, read back through Win32 rather than compared as
    /// bytes: the representation is variable-length.
    fn sid_string(sid: PSID) -> String {
        use windows_sys::Win32::Foundation::LocalFree;
        use windows_sys::Win32::Security::Authorization::ConvertSidToStringSidW;
        let mut raw: *mut u16 = std::ptr::null_mut();
        // SAFETY: `sid` is a live SID owned by an `AppContainer`, and `raw` is a
        // live out-pointer that Win32 allocates with `LocalAlloc`.
        let ok = unsafe { ConvertSidToStringSidW(sid, &mut raw) };
        assert_ne!(ok, 0, "ConvertSidToStringSidW failed");
        let mut units = Vec::new();
        let mut index = 0isize;
        // SAFETY: Win32 returned a NUL-terminated wide string; the loop stops at
        // the terminator and never runs past it.
        unsafe {
            while *raw.offset(index) != 0 {
                units.push(*raw.offset(index));
                index += 1;
            }
            LocalFree(raw as *mut std::ffi::c_void);
        }
        String::from_utf16(&units).expect("utf-16")
    }
}
