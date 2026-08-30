//go:build windows

package windows

import (
	"unsafe"

	xwin "golang.org/x/sys/windows"
)

// Windows API declarations that x/sys/windows does not expose yet.
// All of them are stable, documented since the Vista-era security
// model and unchanged since.

const (
	// disableMaxPrivilege is DISABLE_MAX_PRIVILEGE for
	// CreateRestrictedToken: every privilege is removed from the new
	// token.
	disableMaxPrivilege = 0x1

	// aclRevision is ACL_REVISION for InitializeAcl.
	aclRevision = 2

	// systemMandatoryLabelNoWriteUp is
	// SYSTEM_MANDATORY_LABEL_NO_WRITE_UP: subjects below the object's
	// label cannot write to it.
	systemMandatoryLabelNoWriteUp = 0x1
)

// tokenMandatoryLabel mirrors TOKEN_MANDATORY_LABEL for
// SetTokenInformation(TokenIntegrityLevel).
type tokenMandatoryLabel struct {
	Label xwin.SIDAndAttributes
}

var (
	procCreateRestrictedToken = xwin.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")
	procInitializeAcl         = xwin.NewLazySystemDLL("advapi32.dll").NewProc("InitializeAcl")
	procAddMandatoryAce       = xwin.NewLazySystemDLL("kernel32.dll").NewProc("AddMandatoryAce")
)

// createRestrictedToken wraps CreateRestrictedToken with every
// privilege disabled (DISABLE_MAX_PRIVILEGE) and no SID restrictions.
func createRestrictedToken(existing xwin.Token) (xwin.Token, error) {
	var restricted xwin.Token
	r1, _, e1 := procCreateRestrictedToken.Call(
		uintptr(existing),
		disableMaxPrivilege,
		0, // DisableSidCount
		0, // SidsToDisable
		0, // DeletePrivilegeCount
		0, // PrivilegesToDelete
		0, // RestrictedSidCount
		0, // SidsToRestrict
		uintptr(unsafe.Pointer(&restricted)),
	)
	if r1 == 0 {
		return 0, e1
	}
	return restricted, nil
}

// initializeAcl wraps InitializeAcl with ACL_REVISION.
func initializeAcl(acl *xwin.ACL, length uint32) error {
	r1, _, e1 := procInitializeAcl.Call(
		uintptr(unsafe.Pointer(acl)), uintptr(length), aclRevision)
	if r1 == 0 {
		return e1
	}
	return nil
}

// addMandatoryAce wraps AddMandatoryAce (kernel32): it appends a
// SYSTEM_MANDATORY_LABEL_ACE with the given mandatory policy to acl.
func addMandatoryAce(acl *xwin.ACL, aceFlags, policy uint32, sid *xwin.SID) error {
	r1, _, e1 := procAddMandatoryAce.Call(
		uintptr(unsafe.Pointer(acl)),
		aclRevision,
		uintptr(aceFlags),
		uintptr(policy),
		uintptr(unsafe.Pointer(sid)),
	)
	if r1 == 0 {
		return e1
	}
	return nil
}
