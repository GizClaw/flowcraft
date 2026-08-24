//go:build windows

package windows

import (
	"crypto/rand"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CreateRestrictedToken flags (winnt.h).
const (
	disableMaxPrivilege = 0x1
	luaToken            = 0x4
	writeRestricted     = 0x8
)

// seGroupLogonID is the token-group attribute marking the logon SID.
const seGroupLogonID = 0xC0000000

// tokenAllAccess mirrors codex-rs's requested access for the base
// token: duplicate it into a restricted token, query it, and adjust
// its privileges and default DACL.
const tokenAllAccess = windows.TOKEN_DUPLICATE |
	windows.TOKEN_QUERY |
	windows.TOKEN_ASSIGN_PRIMARY |
	windows.TOKEN_ADJUST_DEFAULT |
	windows.TOKEN_ADJUST_SESSIONID |
	windows.TOKEN_ADJUST_PRIVILEGES

var (
	modadvapi32               = windows.NewLazySystemDLL("advapi32.dll")
	procCreateRestrictedToken = modadvapi32.NewProc("CreateRestrictedToken")
	procSetEntriesInAclW      = modadvapi32.NewProc("SetEntriesInAclW")
	procGetNamedSecurityInfoW = modadvapi32.NewProc("GetNamedSecurityInfoW")
)

// capabilitySIDs are the random per-workspace SIDs that own the
// ACL-based write grants, mirroring codex-rs's cap_sid design. They
// are generated once per Runner: the workspace root and writable roots
// get allow-write ACEs for the workspace SID, and the restricted
// child token carries that SID as a restricting SID, so only paths
// explicitly granted to it are writable (WRITE_RESTRICTED applies the
// restricting list to write checks; reads use the normal token SIDs).
type capabilitySIDs struct {
	workspace string
}

// newCapabilitySIDs generates a fresh workspace capability SID under
// the NT authority (S-1-5-21-...), random so it can never be guessed
// by another process.
func newCapabilitySIDs() (*capabilitySIDs, error) {
	s, err := randomSIDString()
	if err != nil {
		return nil, err
	}
	return &capabilitySIDs{workspace: s}, nil
}

func randomSIDString() (string, error) {
	var words [4]uint32
	if _, err := rand.Read(unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*4)); err != nil {
		return "", fmt.Errorf("windows: generate capability sid: %w", err)
	}
	return fmt.Sprintf("S-1-5-21-%d-%d-%d-%d", words[0], words[1], words[2], words[3]), nil
}

// sidFromString parses a SID string into an allocated SID. The caller
// must release it with releaseSID.
func sidFromString(s string) (*windows.SID, error) {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return nil, err
	}
	var sid *windows.SID
	if err := windows.ConvertStringSidToSid(p, &sid); err != nil {
		return nil, fmt.Errorf("windows: parse sid %q: %w", s, err)
	}
	return sid, nil
}

func releaseSID(sid *windows.SID) {
	if sid != nil {
		_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(sid)))
	}
}

// sidAndAttributes mirrors SID_AND_ATTRIBUTES for the
// CreateRestrictedToken call.
type sidAndAttributes struct {
	sid        *windows.SID
	attributes uint32
}

func createRestrictedToken(base windows.Token, flags uint32, restricted []sidAndAttributes) (windows.Token, error) {
	var newToken windows.Token
	r1, _, e1 := procCreateRestrictedToken.Call(
		uintptr(base),
		uintptr(flags),
		0,
		0,
		0,
		0,
		uintptr(len(restricted)),
		uintptr(unsafe.Pointer(&restricted[0])),
		uintptr(unsafe.Pointer(&newToken)),
	)
	if r1 == 0 {
		return 0, e1
	}
	return newToken, nil
}

// logonSIDFromToken returns the logon SID of the token (a Go-backed
// copy), scanning the token groups and falling back to the linked
// token for elevated processes whose own groups lack it.
func logonSIDFromToken(t windows.Token) (*windows.SID, error) {
	groups, err := t.GetTokenGroups()
	if err != nil {
		return nil, err
	}
	if sid := scanLogonSID(groups); sid != nil {
		return sid, nil
	}
	linked, err := t.GetLinkedToken()
	if err != nil || linked == 0 {
		return nil, fmt.Errorf("windows: logon sid not present on token or linked token")
	}
	defer linked.Close()
	groups, err = linked.GetTokenGroups()
	if err != nil {
		return nil, err
	}
	if sid := scanLogonSID(groups); sid != nil {
		return sid, nil
	}
	return nil, fmt.Errorf("windows: logon sid not present on linked token")
}

// scanLogonSID returns the token's logon SID as a Go-backed copy, or
// nil. x/sys declares Tokengroups.Groups as a fixed one-element array,
// but GetTokenGroups returns a buffer holding GroupCount entries;
// iterate through the real array via unsafe.Slice (indexing the
// declared array past element 0 would panic).
func scanLogonSID(groups *windows.Tokengroups) *windows.SID {
	if groups == nil || groups.GroupCount == 0 {
		return nil
	}
	entries := unsafe.Slice(
		(*windows.SIDAndAttributes)(unsafe.Pointer(&groups.Groups[0])),
		int(groups.GroupCount),
	)
	for i := range entries {
		if entries[i].Attributes&seGroupLogonID != 0 && entries[i].Sid != nil {
			sid, _ := entries[i].Sid.Copy()
			return sid
		}
	}
	return nil
}

// restrictedTokenForSpec derives the restricted token used for every
// spawn of this runner: CreateRestrictedToken over the caller's
// primary token with DISABLE_MAX_PRIVILEGE | LUA_TOKEN |
// WRITE_RESTRICTED, restricted to the workspace capability SID, the
// logon SID, and Everyone. WRITE_RESTRICTED confines writes to paths
// whose DACL grants one of the restricting SIDs (the workspace ACLs)
// while leaving reads on the normal token SIDs; LUA_TOKEN strips the
// Administrators group when the daemon is elevated. SeChangeNotify
// is re-enabled so directory traversal still works.
func restrictedTokenForSpec(caps *capabilitySIDs) (windows.Token, error) {
	var base windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), tokenAllAccess, &base); err != nil {
		return 0, fmt.Errorf("windows: open process token: %w", err)
	}
	defer base.Close()

	logon, err := logonSIDFromToken(base)
	if err != nil {
		return 0, err
	}
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		return 0, fmt.Errorf("windows: create world sid: %w", err)
	}
	capSID, err := sidFromString(caps.workspace)
	if err != nil {
		return 0, err
	}
	defer releaseSID(capSID)

	restricted := []sidAndAttributes{
		{sid: capSID, attributes: 0},
		{sid: logon, attributes: 0},
		{sid: everyone, attributes: 0},
	}
	tok, err := createRestrictedToken(base, disableMaxPrivilege|luaToken|writeRestricted, restricted)
	if err != nil {
		return 0, fmt.Errorf("windows: create restricted token: %w", err)
	}
	// Default DACL: objects the child creates without an explicit DACL
	// (temp files, pipes, named objects) must be usable by the child
	// and by the daemon. Grant GENERIC_ALL to logon + everyone + the
	// workspace capability SID, like codex-rs.
	if err := setTokenDefaultDACL(tok, []*windows.SID{logon, everyone, capSID}); err != nil {
		_ = tok.Close()
		return 0, err
	}
	if err := enablePrivilege(tok, "SeChangeNotifyPrivilege"); err != nil {
		_ = tok.Close()
		return 0, fmt.Errorf("windows: enable change-notify privilege: %w", err)
	}
	return tok, nil
}

// tokenDefaultDacl mirrors TOKEN_DEFAULT_DACL.
type tokenDefaultDacl struct {
	defaultDacl *windows.ACL
}

func setTokenDefaultDACL(t windows.Token, sids []*windows.SID) error {
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for _, sid := range sids {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       0,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	var newACL *windows.ACL
	if err := setEntriesInAcl(entries, nil, &newACL); err != nil {
		return fmt.Errorf("windows: build default dacl: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(newACL)))
	info := tokenDefaultDacl{defaultDacl: newACL}
	if err := windows.SetTokenInformation(
		t,
		windows.TokenDefaultDacl,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return fmt.Errorf("windows: set token default dacl: %w", err)
	}
	return nil
}

func enablePrivilege(t windows.Token, name string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, namePtr, &luid); err != nil {
		return err
	}
	tp := windows.Tokenprivileges{PrivilegeCount: 1}
	tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	return windows.AdjustTokenPrivileges(t, false, &tp, 0, nil, nil)
}
