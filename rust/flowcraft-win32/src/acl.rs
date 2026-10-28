//! What an AppContainer is allowed to touch.
//!
//! The sandbox's identity is a **SID**: an account's, or one capability SID per
//! workspace. Every path a session may reach has to say so in the path's own DACL,
//! and every path it must *not* reach can say that too — which is why this module has
//! both halves.
//!
//! The grant is additive on purpose: `SetEntriesInAclW` merges one entry into the
//! existing DACL, so the user's own access is untouched and a second session
//! granting the same path is a no-op rather than a rewrite. `SUB_CONTAINERS_AND_OBJECTS_INHERIT`
//! makes the entry cover what is created underneath, which is what a workspace is.

use std::path::Path;
use std::ptr::null_mut;

use windows_sys::Win32::Foundation::{ERROR_SUCCESS, LocalFree};
use windows_sys::Win32::Security::Authorization::{
    DENY_ACCESS, EXPLICIT_ACCESS_W, GRANT_ACCESS, GetNamedSecurityInfoW, SE_FILE_OBJECT,
    SetEntriesInAclW, SetNamedSecurityInfoW, TRUSTEE_IS_SID, TRUSTEE_IS_UNKNOWN, TRUSTEE_W,
};
use windows_sys::Win32::Security::{
    ACL, DACL_SECURITY_INFORMATION, PSECURITY_DESCRIPTOR, PSID, SUB_CONTAINERS_AND_OBJECTS_INHERIT,
};
use windows_sys::Win32::Storage::FileSystem::{
    FILE_GENERIC_EXECUTE, FILE_GENERIC_READ, FILE_GENERIC_WRITE,
};

use crate::job::Error;

/// What a granted path may be used for.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Access {
    /// Read and execute: a tool, or a directory the child works *from*.
    Read,
    /// Read, write and execute: the workspace the child is asked to change.
    ReadWrite,
}

impl Access {
    fn mask(self) -> u32 {
        match self {
            Self::Read => FILE_GENERIC_READ | FILE_GENERIC_EXECUTE,
            Self::ReadWrite => FILE_GENERIC_READ | FILE_GENERIC_WRITE | FILE_GENERIC_EXECUTE,
        }
    }
}

/// Allow `sid` to reach `path`, and everything created under it.
///
/// Idempotent: granting twice leaves one entry, because `SetEntriesInAclW` merges
/// by trustee. A failure is not fatal to a daemon but is fatal to the session that
/// asked for it — a path the child cannot open is a session that cannot work.
pub fn grant(path: &Path, sid: PSID, access: Access) -> Result<(), Error> {
    merge(path, sid, GRANT_ACCESS, access.mask())
}

/// Refuse `sid` read on `path`, and on everything created under it.
///
/// A deny entry beats any grant, including one this module added earlier, which is
/// what makes it the tool for "the session may work here but must not read the rest
/// of the user's data". Denying does not remove the user's own access: the entry
/// names one SID.
pub fn deny_read(path: &Path, sid: PSID) -> Result<(), Error> {
    merge(path, sid, DENY_ACCESS, FILE_GENERIC_READ)
}

/// Merge one explicit entry into `path`'s DACL and write it back.
fn merge(path: &Path, sid: PSID, mode: i32, mask: u32) -> Result<(), Error> {
    use std::os::windows::ffi::OsStrExt as _;

    let wide: Vec<u16> = path
        .as_os_str()
        .encode_wide()
        .chain(std::iter::once(0))
        .collect();

    // The DACL as it stands. `descriptor` owns it; `dacl` points into it.
    let mut dacl: *mut ACL = null_mut();
    let mut descriptor: PSECURITY_DESCRIPTOR = null_mut();
    // SAFETY: `wide` is NUL-terminated and outlives the call; the three out-pointers
    // are live, and the ones passed null are documented as optional.
    let status = unsafe {
        GetNamedSecurityInfoW(
            wide.as_ptr(),
            SE_FILE_OBJECT,
            DACL_SECURITY_INFORMATION,
            null_mut(),
            null_mut(),
            &mut dacl,
            null_mut(),
            &mut descriptor,
        )
    };
    if status != ERROR_SUCCESS {
        return Err(Error {
            code: Some(status),
            message: format!("sandbox: cannot read the ACL of {}", path.display()),
        });
    }
    // SAFETY: the descriptor came from the call above and is freed once, below.
    let descriptor = Descriptor(descriptor);

    let entry = EXPLICIT_ACCESS_W {
        grfAccessPermissions: mask,
        grfAccessMode: mode,
        grfInheritance: SUB_CONTAINERS_AND_OBJECTS_INHERIT,
        Trustee: TRUSTEE_W {
            pMultipleTrustee: null_mut(),
            // `NO_MULTIPLE_TRUSTEE`: the field is unused unless a multi-trustee form
            // is requested, and zero is that value.
            MultipleTrusteeOperation: 0,
            TrusteeForm: TRUSTEE_IS_SID,
            TrusteeType: TRUSTEE_IS_UNKNOWN,
            // `ptstrName` holds the trustee's SID for `TRUSTEE_IS_SID`, not a string
            // — the field is `PWSTR` for historical reasons only.
            ptstrName: sid as *mut u16,
        },
    };

    let mut merged: *mut ACL = null_mut();
    // SAFETY: one entry, a live old DACL, and a live out-pointer; the result is a
    // freshly allocated ACL that this function frees.
    let status = unsafe { SetEntriesInAclW(1, &entry, dacl, &mut merged) };
    if status != ERROR_SUCCESS {
        return Err(Error {
            code: Some(status),
            message: format!("sandbox: cannot merge a grant for {}", path.display()),
        });
    }
    let merged = Descriptor(merged.cast());

    // SAFETY: the path is NUL-terminated, the DACL is live, and the owner/group/sacl
    // arguments are untouched by passing null.
    let status = unsafe {
        SetNamedSecurityInfoW(
            wide.as_ptr(),
            SE_FILE_OBJECT,
            DACL_SECURITY_INFORMATION,
            null_mut(),
            null_mut(),
            merged.0.cast::<ACL>(),
            null_mut(),
        )
    };
    if status != ERROR_SUCCESS {
        return Err(Error {
            code: Some(status),
            message: format!("sandbox: cannot write the ACL of {}", path.display()),
        });
    }
    drop(descriptor);
    Ok(())
}

/// Grant `path`, and read on every ancestor the walk to it passes through.
///
/// A grant on a leaf is not enough, and the symptom misleads: opening
/// `C:\a\b\c.exe` walks `a` and `b` first, and a container that was granted only
/// `b` is refused where the walk starts — which the image loader reports as
/// `ERROR_FILE_NOT_FOUND` on a path that plainly exists. Read on an ancestor
/// inherits *read* down, so it cannot widen a write grant that was not asked for;
/// permission on the drive root itself is left untouched.
pub fn grant_with_ancestors(path: &Path, sid: PSID, access: Access) -> Result<(), Error> {
    grant(path, sid, access)?;
    let mut current = path.parent();
    while let Some(dir) = current {
        // Stop before the volume root: `C:\` is not a directory this crate should
        // rewrite the ACL of.
        if dir.parent().is_none() {
            break;
        }
        grant(dir, sid, Access::Read)?;
        current = dir.parent();
    }
    Ok(())
}

/// A buffer Win32 allocated for us, freed exactly once.
struct Descriptor(PSECURITY_DESCRIPTOR);

// SAFETY: the pointer is an allocation with no thread affinity, touched only by this
// type's `Drop`.
unsafe impl Send for Descriptor {}

impl Drop for Descriptor {
    fn drop(&mut self) {
        if !self.0.is_null() {
            // SAFETY: allocated by `GetNamedSecurityInfoW`/`SetEntriesInAclW`, both
            // documented as `LocalAlloc`, and not freed anywhere else.
            unsafe { LocalFree(self.0) };
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A SID this process holds, copied out of its own token so the buffer outlives
    /// the token query.
    struct OwnedSid(Vec<u64>);

    impl OwnedSid {
        fn current_user() -> Self {
            use windows_sys::Win32::Security::{CopySid, GetLengthSid, TOKEN_QUERY, TokenUser};
            use windows_sys::Win32::System::Threading::{GetCurrentProcess, OpenProcessToken};

            let mut token = std::ptr::null_mut();
            // SAFETY: the pseudo-handle is always valid and the out-pointer is live.
            let opened = unsafe { OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &mut token) };
            assert_ne!(opened, 0, "OpenProcessToken failed");

            let mut needed: u32 = 0;
            // SAFETY: a null buffer with a live size slot is how the required length
            // is asked for; the call is expected to fail.
            let sized = unsafe {
                windows_sys::Win32::Security::GetTokenInformation(
                    token,
                    TokenUser,
                    std::ptr::null_mut(),
                    0,
                    &mut needed,
                )
            };
            assert_eq!(sized, 0);
            assert!(needed > 0);

            let mut buffer = vec![0u64; needed.div_ceil(8) as usize];
            // SAFETY: the buffer is at least `needed` bytes and lives across the call.
            let filled = unsafe {
                windows_sys::Win32::Security::GetTokenInformation(
                    token,
                    TokenUser,
                    buffer.as_mut_ptr() as *mut std::ffi::c_void,
                    needed,
                    &mut needed,
                )
            };
            assert_ne!(filled, 0, "GetTokenInformation(TokenUser) failed");

            // SAFETY: `TokenUser` filled a `TOKEN_USER` at the buffer's start.
            let user = unsafe {
                *buffer
                    .as_ptr()
                    .cast::<windows_sys::Win32::Security::TOKEN_USER>()
            };
            let source = user.User.Sid;
            // SAFETY: `source` points into `buffer`, which is alive.
            let length = unsafe { GetLengthSid(source) };
            let mut copy = vec![0u64; (length as usize).div_ceil(8)];
            // SAFETY: the destination is at least `length` bytes.
            let copied = unsafe { CopySid(length, copy.as_mut_ptr().cast(), source) };
            assert_ne!(copied, 0, "CopySid failed");
            // SAFETY: the handle came from `OpenProcessToken` and is closed once.
            unsafe { windows_sys::Win32::Foundation::CloseHandle(token) };
            Self(copy)
        }

        fn as_ptr(&self) -> PSID {
            self.0.as_ptr().cast_mut().cast()
        }
    }

    /// The DACL of `path`, as explicit entries.
    fn entries(path: &Path) -> Vec<EXPLICIT_ACCESS_W> {
        use std::os::windows::ffi::OsStrExt as _;
        use windows_sys::Win32::Security::Authorization::GetExplicitEntriesFromAclW;

        let wide: Vec<u16> = path
            .as_os_str()
            .encode_wide()
            .chain(std::iter::once(0))
            .collect();
        let mut dacl: *mut ACL = null_mut();
        let mut descriptor: PSECURITY_DESCRIPTOR = null_mut();
        // SAFETY: as in `merge`.
        let status = unsafe {
            GetNamedSecurityInfoW(
                wide.as_ptr(),
                SE_FILE_OBJECT,
                DACL_SECURITY_INFORMATION,
                null_mut(),
                null_mut(),
                &mut dacl,
                null_mut(),
                &mut descriptor,
            )
        };
        assert_eq!(status, ERROR_SUCCESS, "read back {}", path.display());
        let descriptor = Descriptor(descriptor);
        let mut list: *mut EXPLICIT_ACCESS_W = null_mut();
        let mut count: u32 = 0;
        // SAFETY: the DACL is live and the two out-pointers are live.
        let status = unsafe { GetExplicitEntriesFromAclW(dacl, &mut count, &mut list) };
        assert_eq!(status, ERROR_SUCCESS, "enumerate {}", path.display());
        let entries = if list.is_null() || count == 0 {
            Vec::new()
        } else {
            // SAFETY: the call reported `count` entries at `list`.
            unsafe { std::slice::from_raw_parts(list, count as usize).to_vec() }
        };
        if !list.is_null() {
            // SAFETY: allocated by `GetExplicitEntriesFromAclW`.
            unsafe { LocalFree(list.cast()) };
        }
        drop(descriptor);
        entries
    }

    fn names(entry: &EXPLICIT_ACCESS_W, sid: PSID) -> bool {
        // SAFETY: `ptstrName` holds a SID for `TRUSTEE_IS_SID`, and both are live.
        unsafe { windows_sys::Win32::Security::EqualSid(entry.Trustee.ptstrName.cast(), sid) != 0 }
    }

    /// A root directory for one test, with its two child directories.
    fn fixture(tag: &str) -> std::path::PathBuf {
        let root =
            std::env::temp_dir().join(format!("flowcraft.acl.{}.{}", tag, std::process::id()));
        std::fs::create_dir_all(root.join("granted")).expect("granted dir");
        std::fs::create_dir_all(root.join("other")).expect("other dir");
        root
    }

    /// A grant is one entry on the directory asked for, and on no other.
    #[test]
    fn a_grant_is_a_new_entry_for_the_sid() {
        let sid = OwnedSid::current_user();
        let root = fixture("grant");
        let (granted, other) = (root.join("granted"), root.join("other"));

        grant(&granted, sid.as_ptr(), Access::ReadWrite).expect("grant");

        // Compared by value, not by pointer: `GetExplicitEntriesFromAclW` hands back
        // copies of the trustee SIDs inside its own allocation.
        assert!(
            entries(&granted)
                .iter()
                .any(|entry| names(entry, sid.as_ptr())),
            "the granted directory must carry an entry for the SID"
        );
        assert!(
            !entries(&other)
                .iter()
                .any(|entry| names(entry, sid.as_ptr())),
            "a directory that was not granted must not carry one"
        );
        let _ = std::fs::remove_dir_all(&root);
    }

    /// The deny half: an entry for our own SID stops this process reading.
    ///
    /// A refusal is only worth asserting against an identity that actually presents
    /// the SID, and the one every test process certainly holds is its own.
    #[test]
    fn a_deny_stops_the_sid_reading() {
        let sid = OwnedSid::current_user();
        let root = fixture("deny");
        let denied = root.join("denied");
        std::fs::create_dir_all(&denied).expect("denied dir");
        std::fs::write(denied.join("secret.txt"), b"x").expect("write");

        deny_read(&denied, sid.as_ptr()).expect("deny read");
        assert!(
            entries(&denied)
                .iter()
                .any(|entry| names(entry, sid.as_ptr())),
            "the deny entry must be in the DACL"
        );
        let refused = std::fs::read_dir(&denied);
        assert!(
            refused.is_err(),
            "a denied SID must not be able to list the directory"
        );

        // Best effort: the deny names us, so removing the tree may also be refused.
        let _ = std::fs::remove_dir_all(&root);
    }
}
