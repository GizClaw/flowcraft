//go:build windows

package windows

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"golang.org/x/sys/windows"
)

// fileDeleteChild is the FILE_DELETE_CHILD access right (0x40), absent
// from x/sys's constant table.
const fileDeleteChild = 0x00000040

// writeAllowMask is the allow mask granted to the workspace capability
// SID on the runner root and writable roots, mirroring codex-rs:
// full read/write/execute plus DELETE, but deliberately excluding
// FILE_DELETE_CHILD so the sandboxed process cannot delete entries
// inside protected subdirectories through the parent's write ACE.
const writeAllowMask = windows.FILE_GENERIC_READ |
	windows.FILE_GENERIC_WRITE |
	windows.FILE_GENERIC_EXECUTE |
	windows.DELETE

// denyWriteMask is what a deny ACE for the capability SID covers.
// WRITE_DAC / WRITE_OWNER are included so the file owner (the same
// user, under the restricted token) cannot simply re-grant itself
// access to a protected path.
const denyWriteMask = windows.FILE_GENERIC_WRITE |
	windows.FILE_WRITE_DATA |
	windows.FILE_APPEND_DATA |
	windows.FILE_WRITE_EA |
	windows.FILE_WRITE_ATTRIBUTES |
	0x40000000 | // GENERIC_WRITE
	windows.DELETE |
	fileDeleteChild |
	windows.WRITE_DAC |
	windows.WRITE_OWNER

// setEntriesInAcl wraps advapi32.SetEntriesInAclW (x/sys keeps its
// binding private). It builds a new ACL from oldACL plus entries; the
// caller must LocalFree the returned ACL.
func setEntriesInAcl(entries []windows.EXPLICIT_ACCESS, oldACL *windows.ACL, newACL **windows.ACL) error {
	r1, _, e1 := procSetEntriesInAclW.Call(
		uintptr(len(entries)),
		uintptr(unsafe.Pointer(&entries[0])),
		uintptr(unsafe.Pointer(oldACL)),
		uintptr(unsafe.Pointer(newACL)),
	)
	if r1 != 0 {
		return e1
	}
	return nil
}

// getDACL fetches the DACL of an existing path (SE_FILE_OBJECT). The
// returned security descriptor must be LocalFree'd; the dacl pointer
// lives inside it.
func getDACL(path string) (dacl *windows.ACL, sd *windows.SECURITY_DESCRIPTOR, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, nil, err
	}
	r1, _, e1 := procGetNamedSecurityInfoW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(windows.SE_FILE_OBJECT),
		uintptr(windows.DACL_SECURITY_INFORMATION),
		0,
		0,
		uintptr(unsafe.Pointer(&dacl)),
		0,
		uintptr(unsafe.Pointer(&sd)),
	)
	if r1 != 0 {
		return nil, nil, e1
	}
	return dacl, sd, nil
}

// aclHasAllowForSID reports whether an allow ACE for sid already
// grants every bit of allowMask and none of disallowMask, so ACL
// application is idempotent across sessions.
func aclHasAllowForSID(acl *windows.ACL, sid *windows.SID, allowMask, disallowMask uint32) bool {
	if acl == nil {
		return false
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !windows.EqualSid(aceSID, sid) {
			continue
		}
		mask := uint32(ace.Mask)
		if mask&allowMask == allowMask && mask&disallowMask == 0 {
			return true
		}
	}
	return false
}

// aclHasDenyForSID reports whether a deny ACE for sid overlaps any of
// denyMask.
func aclHasDenyForSID(acl *windows.ACL, sid *windows.SID, denyMask uint32) bool {
	if acl == nil {
		return false
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !windows.EqualSid(aceSID, sid) {
			continue
		}
		if uint32(ace.Mask)&denyMask != 0 {
			return true
		}
	}
	return false
}

// ensureAllowWriteACE grants sid the writeAllowMask on path with
// container+object inheritance, skipping the rewrite when an adequate
// ACE already exists. Returns whether anything changed.
func ensureAllowWriteACE(path string, sid *windows.SID) (bool, error) {
	oldACL, sd, err := getDACL(path)
	if err != nil {
		return false, fmt.Errorf("windows: read dacl of %q: %w", path, err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(sd)))
	if aclHasAllowForSID(oldACL, sid, writeAllowMask, fileDeleteChild) {
		return false, nil
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: writeAllowMask,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.CONTAINER_INHERIT_ACE | windows.OBJECT_INHERIT_ACE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}
	var newACL *windows.ACL
	if err := setEntriesInAcl(entries, oldACL, &newACL); err != nil {
		return false, fmt.Errorf("windows: build dacl for %q: %w", path, err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(newACL)))
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
		nil,
		nil,
		newACL,
		nil,
	); err != nil {
		return false, fmt.Errorf("windows: apply dacl to %q: %w", path, err)
	}
	return true, nil
}

// addDenyWriteACE adds a deny ACE for sid on path with inheritance,
// skipping when one already exists. Deny ACEs precede allows in the
// DACL, so protected subdirectories stay read-only-for-write even
// under the parent's allow-write ACE.
func addDenyWriteACE(path string, sid *windows.SID) (bool, error) {
	oldACL, sd, err := getDACL(path)
	if err != nil {
		return false, fmt.Errorf("windows: read dacl of %q: %w", path, err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(sd)))
	if aclHasDenyForSID(oldACL, sid, denyWriteMask) {
		return false, nil
	}
	entries := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: denyWriteMask,
		AccessMode:        windows.DENY_ACCESS,
		Inheritance:       windows.CONTAINER_INHERIT_ACE | windows.OBJECT_INHERIT_ACE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}
	var newACL *windows.ACL
	if err := setEntriesInAcl(entries, oldACL, &newACL); err != nil {
		return false, fmt.Errorf("windows: build deny dacl for %q: %w", path, err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(newACL)))
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
		nil,
		nil,
		newACL,
		nil,
	); err != nil {
		return false, fmt.Errorf("windows: apply deny dacl to %q: %w", path, err)
	}
	return true, nil
}

// applyWorkspaceACLs grants the workspace capability SID write access
// to the runner root and every writable root. Called once at Runner
// construction; repeated calls are idempotent (existing adequate ACEs
// short-circuit the rewrite).
func applyWorkspaceACLs(root string, writable []string, caps *capabilitySIDs) error {
	sid, err := sidFromString(caps.workspace)
	if err != nil {
		return err
	}
	defer releaseSID(sid)
	seen := map[string]bool{}
	for _, p := range append([]string{root}, writable...) {
		clean := filepath.Clean(p)
		if seen[clean] {
			continue
		}
		seen[clean] = true
		if _, err := ensureAllowWriteACE(clean, sid); err != nil {
			return err
		}
	}
	return nil
}

// applyProtectedDenies adds deny-write ACEs for the workspace
// capability SID on the agent-owned subdirectories of workDir
// (.codex / .agents), so the sandboxed process cannot rewrite its own
// policy even though it can write the workspace. Non-existent
// subdirectories are skipped, mirroring codex-rs.
func applyProtectedDenies(workDir string, caps *capabilitySIDs) error {
	sid, err := sidFromString(caps.workspace)
	if err != nil {
		return err
	}
	defer releaseSID(sid)
	return applyProtectedDeniesSID(workDir, sid)
}

// applyProtectedDeniesSID adds deny-write ACEs for sid on the
// agent-owned subdirectories of workDir. It is shared by the
// unelevated path (workspace capability SID) and the elevated path
// (sandbox account SID).
func applyProtectedDeniesSID(workDir string, sid *windows.SID) error {
	for _, sub := range []string{".codex", ".agents"} {
		p := filepath.Join(workDir, sub)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			if _, err := addDenyWriteACE(p, sid); err != nil {
				return err
			}
		}
	}
	return nil
}

// addDenyReadACE stays NotAvailable in P1: the unelevated
// restricted-token backend cannot enforce split read restrictions
// directly (WRITE_RESTRICTED only confines writes; capability SID
// deny-read ACEs do not participate in read checks), exactly as in
// codex-rs. Read carve-outs require the elevated backend (P2).
func addDenyReadACE(string, *windows.SID) error {
	return errdefs.NotAvailablef(
		"windows: deny-read ACE requires the elevated backend (P2)")
}
