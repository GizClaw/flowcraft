//! What an AppContainer is allowed to touch.
//!
//! A container's SID owns nothing: every path it may read or write has to say so in
//! its own DACL. That is the *strength* of the boundary — the grant is the complete
//! list — and also the work: a session that is asked to work in a directory must
//! have that directory and its tools granted before the child starts, or the child
//! cannot even load itself.
//!
//! The grant is additive on purpose: `SetEntriesInAclW` merges one entry into the
//! existing DACL, so the user's own access is untouched and a second session
//! granting the same path is a no-op rather than a rewrite. `SUB_CONTAINERS_AND_OBJECTS_INHERIT`
//! makes the entry cover what is created underneath, which is what a workspace is.

use std::path::Path;
use std::ptr::null_mut;

use windows_sys::Win32::Foundation::{ERROR_SUCCESS, LocalFree};
use windows_sys::Win32::Security::Authorization::{
    EXPLICIT_ACCESS_W, GRANT_ACCESS, GetNamedSecurityInfoW, SE_FILE_OBJECT, SetEntriesInAclW,
    SetNamedSecurityInfoW, TRUSTEE_IS_SID, TRUSTEE_IS_UNKNOWN, TRUSTEE_W,
};
use windows_sys::Win32::Security::{
    ACL, DACL_SECURITY_INFORMATION, PSECURITY_DESCRIPTOR, SUB_CONTAINERS_AND_OBJECTS_INHERIT,
};
use windows_sys::Win32::Storage::FileSystem::{
    FILE_GENERIC_EXECUTE, FILE_GENERIC_READ, FILE_GENERIC_WRITE,
};

use crate::appcontainer::AppContainer;
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

/// Allow `container` to reach `path`, and everything created under it.
///
/// Idempotent: granting twice leaves one entry, because `SetEntriesInAclW` merges
/// by trustee. A failure is not fatal to a daemon but is fatal to the session that
/// asked for it — a path the child cannot open is a session that cannot work.
pub fn grant(path: &Path, container: &AppContainer, access: Access) -> Result<(), Error> {
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
        grfAccessPermissions: access.mask(),
        grfAccessMode: GRANT_ACCESS,
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
            ptstrName: container.sid() as *mut u16,
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

    /// A container, and two directories: one granted, one not.
    fn fixture(tag: &str) -> (AppContainer, std::path::PathBuf, std::path::PathBuf) {
        let name = format!("flowcraft.acl.{}.{}", tag, std::process::id());
        let container = AppContainer::open(&name).expect("profile");
        let root =
            std::env::temp_dir().join(format!("flowcraft.acl.{}.{}", tag, std::process::id()));
        let granted = root.join("granted");
        let other = root.join("other");
        std::fs::create_dir_all(&granted).expect("granted dir");
        std::fs::create_dir_all(&other).expect("other dir");
        (container, granted, other)
    }

    /// The grant reaches the directory's DACL, and only the directory asked for.
    #[test]
    fn a_grant_is_a_new_entry_for_the_container_sid() {
        use windows_sys::Win32::Security::Authorization::{
            EXPLICIT_ACCESS_W, GetExplicitEntriesFromAclW,
        };

        let (container, granted, other) = fixture("entry");
        grant(&granted, &container, Access::ReadWrite).expect("grant");

        let entries = |path: &std::path::Path| -> Vec<EXPLICIT_ACCESS_W> {
            use std::os::windows::ffi::OsStrExt as _;
            let wide: Vec<u16> = path
                .as_os_str()
                .encode_wide()
                .chain(std::iter::once(0))
                .collect();
            let mut dacl: *mut ACL = null_mut();
            let mut descriptor: PSECURITY_DESCRIPTOR = null_mut();
            // SAFETY: as in `grant`.
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
            // SAFETY: the DACL is live, and the two out-pointers are live.
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
        };

        // Compared by value, not by pointer: `GetExplicitEntriesFromAclW` hands back
        // copies of the trustee SIDs inside its own allocation, so a pointer
        // comparison can only ever be false.
        let names = |entry: &EXPLICIT_ACCESS_W| -> bool {
            // SAFETY: `ptstrName` holds a SID for `TRUSTEE_IS_SID`, and both SIDs are
            // live for the duration of the call.
            unsafe {
                windows_sys::Win32::Security::EqualSid(
                    entry.Trustee.ptstrName.cast(),
                    container.sid(),
                ) != 0
            }
        };
        assert!(
            entries(&granted).iter().any(names),
            "the granted directory must carry an entry for the container"
        );
        assert!(
            !entries(&other).iter().any(names),
            "a directory that was not granted must not carry one"
        );

        let _ = std::fs::remove_dir_all(other.parent().expect("root"));
        AppContainer::delete(&name(&container)).expect("delete");
    }

    /// The profile's name, recovered for cleanup.
    fn name(container: &AppContainer) -> String {
        container.name().to_owned()
    }
}
