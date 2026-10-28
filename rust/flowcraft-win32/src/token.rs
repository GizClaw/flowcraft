//! Security tokens: what makes a child *less* privileged than its parent.
//!
//! The sandbox's write boundary on Windows is an **integrity level**, not an
//! access-control list. Windows labels every securable object with an integrity
//! level and enforces "no write up": a process at Low cannot write to an object
//! labelled Medium even when its ACL grants access. Giving a child a Low token is
//! therefore a *write* boundary that needs no per-path ACL rewriting, which is why
//! this phase uses it.
//!
//! What it does not do: it does not restrict reads, and it does not stop a
//! low-integrity process from writing to Low-labelled places (`%TEMP%`, anything it
//! creates itself). That is the point — the boundary is "cannot damage the user's
//! data", not "cannot run".
//!
//! The token is derived from the caller's own: `OpenProcessToken` →
//! `DuplicateTokenEx(TokenPrimary)` → `SetTokenInformation(TokenIntegrityLevel)`.
//! A duplicate of the caller's own token that is restricted further needs no
//! `SeAssignPrimaryTokenPrivilege` in `CreateProcessAsUser`, which is what keeps
//! the whole thing unelevated.
//!
//! The integrity SID is built by hand (`S-1-16-<rid>`: revision 1, one
//! sub-authority, authority 16) instead of going through `ConvertStringSidToSidW`,
//! because that path allocates, needs `LocalFree`, and turns a 12-byte constant
//! into a string-to-binary parse on every session.

use std::ffi::c_void;

use windows_sys::Win32::Foundation::{CloseHandle, HANDLE};
use windows_sys::Win32::Security::{
    DuplicateTokenEx, GetTokenInformation, SecurityImpersonation, SetTokenInformation,
    TOKEN_ADJUST_DEFAULT, TOKEN_ASSIGN_PRIMARY, TOKEN_DUPLICATE, TOKEN_MANDATORY_LABEL,
    TOKEN_QUERY, TokenIntegrityLevel, TokenPrimary,
};
use windows_sys::Win32::System::Threading::{GetCurrentProcess, OpenProcessToken};

use crate::job::Error;

/// `SE_GROUP_INTEGRITY`: the attribute that makes a SID an integrity label.
/// `windows-sys` exposes the struct but not this flag.
const SE_GROUP_INTEGRITY: u32 = 0x0000_0020;

/// `SECURITY_MANDATORY_LABEL_AUTHORITY`: the `16` in `S-1-16-<rid>`.
const MANDATORY_LABEL_AUTHORITY: u64 = 16;

/// The integrity levels Windows enforces.
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub enum Integrity {
    /// Below Low: content a browser would not trust either.
    Untrusted,
    /// What a sandboxed child gets: no write up.
    Low,
    /// An ordinary user process.
    Medium,
    /// Elevated.
    High,
    /// Kernel and system components.
    System,
}

impl Integrity {
    /// The `S-1-16-<rid>` sub-authority for this level.
    const fn rid(self) -> u32 {
        match self {
            Self::Untrusted => 0,
            Self::Low => 4096,
            Self::Medium => 8192,
            Self::High => 12288,
            Self::System => 16384,
        }
    }

    /// Decode a RID a token reported.
    const fn from_rid(rid: u32) -> Self {
        match rid {
            0 => Self::Untrusted,
            1..=4096 => Self::Low,
            4097..=8192 => Self::Medium,
            8193..=12288 => Self::High,
            _ => Self::System,
        }
    }

    /// The SID bytes for this level: `S-1-16-<rid>`.
    fn sid(self) -> [u8; 12] {
        let mut sid = [0u8; 12];
        sid[0] = 1; // revision
        sid[1] = 1; // sub-authority count
        // A 48-bit big-endian identifier authority.
        sid[2..8].copy_from_slice(&MANDATORY_LABEL_AUTHORITY.to_be_bytes()[2..]);
        sid[8..12].copy_from_slice(&self.rid().to_le_bytes());
        sid
    }
}

/// A primary token a child can be started with.
///
/// Dropping it closes the handle; a running child keeps its own reference.
pub struct Token {
    handle: HANDLE,
}

// SAFETY: a token handle is a kernel object with no thread affinity; Win32
// documents every call used here as callable from any thread, and the handle is
// closed exactly once by `Drop`.
unsafe impl Send for Token {}
// SAFETY: as above.
unsafe impl Sync for Token {}

impl std::fmt::Debug for Token {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("flowcraft_win32::Token")
    }
}

/// The rights this crate asks for: duplicate a token, assign it as a primary one,
/// read it back, and change its integrity label.
const TOKEN_ACCESS: u32 =
    TOKEN_DUPLICATE | TOKEN_ASSIGN_PRIMARY | TOKEN_QUERY | TOKEN_ADJUST_DEFAULT;

impl Token {
    /// A primary token equal to this process's own, before any restriction.
    fn duplicate() -> Result<Self, Error> {
        let mut process: HANDLE = std::ptr::null_mut();
        // SAFETY: the pseudo-handle is always valid and the out-pointer is live.
        let ok = unsafe { OpenProcessToken(GetCurrentProcess(), TOKEN_ACCESS, &mut process) };
        if ok == 0 {
            return Err(Error::last("sandbox: OpenProcessToken failed"));
        }
        let process = Handle(process);

        let mut token: HANDLE = std::ptr::null_mut();
        // SAFETY: the source token is live, and this asks for a primary token with
        // the four rights above.
        let ok = unsafe {
            DuplicateTokenEx(
                process.raw(),
                TOKEN_ACCESS,
                std::ptr::null(),
                SecurityImpersonation,
                TokenPrimary,
                &mut token,
            )
        };
        if ok == 0 {
            return Err(Error::last("sandbox: DuplicateTokenEx failed"));
        }
        Ok(Self { handle: token })
    }

    /// Derive a token at `level` from this process's own token.
    pub fn derive(level: Integrity) -> Result<Self, Error> {
        let token = Self::duplicate()?;
        token.set_integrity(level)?;
        Ok(token)
    }

    /// The integrity level currently on this token.
    pub fn integrity(&self) -> Result<Integrity, Error> {
        let mut needed: u32 = 0;
        // SAFETY: a null buffer with a live size slot is the documented way to ask
        // for the required length.
        unsafe {
            GetTokenInformation(
                self.handle,
                TokenIntegrityLevel,
                std::ptr::null_mut(),
                0,
                &mut needed,
            )
        };
        if needed == 0 {
            return Err(Error::last(
                "sandbox: the token has no integrity level to report",
            ));
        }
        let mut buffer = vec![0u64; usize::try_from(needed).unwrap_or(0).div_ceil(8)];
        // SAFETY: the buffer is at least `needed` bytes and outlives the call.
        let ok = unsafe {
            GetTokenInformation(
                self.handle,
                TokenIntegrityLevel,
                buffer.as_mut_ptr().cast::<c_void>(),
                needed,
                &mut needed,
            )
        };
        if ok == 0 {
            return Err(Error::last("sandbox: GetTokenInformation failed"));
        }
        // SAFETY: the kernel wrote a `TOKEN_MANDATORY_LABEL` at the start of the
        // buffer, and its `Sid` points into that same buffer (twelve bytes for the
        // SIDs this module creates).
        let label = unsafe { &*(buffer.as_ptr().cast::<TOKEN_MANDATORY_LABEL>()) };
        // SAFETY: layout of a SID: revision, sub-authority count, six-byte
        // authority, then `count` little-endian sub-authorities. The last one is
        // the integrity RID.
        let rid = unsafe {
            let count = *label.Label.Sid.cast::<u8>().add(1);
            let base = label.Label.Sid.cast::<u8>().add(8).cast::<u32>();
            *base.add(usize::from(count).saturating_sub(1))
        };
        Ok(Integrity::from_rid(rid))
    }

    /// The raw handle, for `CreateProcessAsUserW`.
    pub(crate) fn raw(&self) -> HANDLE {
        self.handle
    }

    fn set_integrity(&self, level: Integrity) -> Result<(), Error> {
        let sid = level.sid();
        let label = TOKEN_MANDATORY_LABEL {
            Label: windows_sys::Win32::Security::SID_AND_ATTRIBUTES {
                Sid: sid.as_ptr().cast_mut().cast::<c_void>(),
                Attributes: SE_GROUP_INTEGRITY,
            },
        };
        let size = u32::try_from(std::mem::size_of::<TOKEN_MANDATORY_LABEL>()).unwrap_or(u32::MAX);
        // SAFETY: the label is the documented shape for `TokenIntegrityLevel`, the
        // SID it points at outlives the call, and `size` is the struct's size.
        let ok = unsafe {
            SetTokenInformation(
                self.handle,
                TokenIntegrityLevel,
                (&raw const label).cast::<c_void>(),
                size,
            )
        };
        if ok == 0 {
            return Err(Error::last(
                "sandbox: SetTokenInformation(TokenIntegrityLevel) failed",
            ));
        }
        Ok(())
    }
}

impl Drop for Token {
    fn drop(&mut self) {
        // SAFETY: the handle came from `DuplicateTokenEx` and is closed once.
        unsafe { CloseHandle(self.handle) };
    }
}

/// A process token handle, closed on drop.
struct Handle(HANDLE);

impl Handle {
    fn raw(&self) -> HANDLE {
        self.0
    }
}

impl Drop for Handle {
    fn drop(&mut self) {
        // SAFETY: the handle came from `OpenProcessToken` and is closed once.
        unsafe { CloseHandle(self.0) };
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_derived_token_reports_the_level_it_was_given() {
        for level in [Integrity::Low, Integrity::Medium] {
            let token = Token::derive(level).expect("derive");
            assert_eq!(token.integrity().expect("integrity"), level, "{level:?}");
        }
    }

    #[test]
    fn a_low_token_can_be_derived_twice() {
        // Two sessions need two tokens; the intermediate handle is closed each time.
        let first = Token::derive(Integrity::Low).expect("first");
        let second = Token::derive(Integrity::Low).expect("second");
        assert_eq!(first.integrity().expect("first"), Integrity::Low);
        assert_eq!(second.integrity().expect("second"), Integrity::Low);
    }

    #[test]
    fn the_sid_layout_matches_s_1_16() {
        let sid = Integrity::Low.sid();
        assert_eq!(sid[0], 1, "revision");
        assert_eq!(sid[1], 1, "one sub-authority");
        assert_eq!(
            u64::from_be_bytes([0, 0, sid[2], sid[3], sid[4], sid[5], sid[6], sid[7]]),
            16
        );
        assert_eq!(u32::from_le_bytes([sid[8], sid[9], sid[10], sid[11]]), 4096);
    }
}
