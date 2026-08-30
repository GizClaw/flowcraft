//go:build windows

package windows

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/GizClaw/flowcraft/core/errdefs"
	xwin "golang.org/x/sys/windows"
)

// lowIntegritySID returns the well-known Low mandatory integrity SID
// (S-1-16-4096). A subject at Low integrity can read Medium-labeled
// objects but cannot write to them, regardless of DACL grants.
func lowIntegritySID() (*xwin.SID, error) {
	name, err := xwin.UTF16PtrFromString("S-1-16-4096")
	if err != nil {
		return nil, errdefs.Internal(fmt.Errorf("windows: encode low integrity sid: %w", err))
	}
	var sid *xwin.SID
	err = xwin.ConvertStringSidToSid(name, &sid)
	if err != nil {
		return nil, errdefs.Internal(fmt.Errorf("windows: convert low integrity sid: %w", err))
	}
	return sid, nil
}

// restrictToken builds the primary token for a write-confined child:
// a restricted version of the caller's own token (every privilege
// disabled) whose integrity level is lowered to Low. CreateProcessAsUser
// accepts a restricted version of the caller's primary token without
// SE_ASSIGNPRIMARYTOKEN_NAME, so this works for ordinary (non-admin)
// applications.
func restrictToken() (xwin.Token, error) {
	var current xwin.Token
	if err := xwin.OpenProcessToken(xwin.CurrentProcess(), xwin.TOKEN_ALL_ACCESS, &current); err != nil {
		return 0, errdefs.Internal(fmt.Errorf("windows: open current token: %w", err))
	}
	defer func() { _ = current.Close() }()

	var primary xwin.Token
	if err := xwin.DuplicateTokenEx(current, xwin.TOKEN_ALL_ACCESS, nil,
		xwin.SecurityImpersonation, xwin.TokenPrimary, &primary); err != nil {
		return 0, errdefs.Internal(fmt.Errorf("windows: duplicate token: %w", err))
	}
	defer func() { _ = primary.Close() }()

	// Lower the integrity level before restricting: the restricted
	// handle's access rights mirror the source, but setting the label
	// on the primary first avoids any TOKEN_ADJUST_DEFAULT question on
	// the restricted token.
	lowSID, err := lowIntegritySID()
	if err != nil {
		return 0, err
	}
	label := tokenMandatoryLabel{
		Label: xwin.SIDAndAttributes{Sid: lowSID, Attributes: xwin.SE_GROUP_INTEGRITY},
	}
	if err := xwin.SetTokenInformation(primary, uint32(xwin.TokenIntegrityLevel),
		(*byte)(unsafe.Pointer(&label)), uint32(unsafe.Sizeof(label))); err != nil {
		return 0, errdefs.Internal(fmt.Errorf("windows: set low integrity on token: %w", err))
	}

	restricted, err := createRestrictedToken(primary)
	if err != nil {
		return 0, errdefs.Internal(fmt.Errorf("windows: restrict token: %w", err))
	}
	return restricted, nil
}

// enableCreateProcessAsUserPrivileges enables the privileges
// CreateProcessAsUser requires on the current process token:
// SE_INCREASE_QUOTA_NAME (always required) and
// SE_ASSIGNPRIMARYTOKEN_NAME (required when the restricted-own-token
// carve-out does not apply). Admin tokens carry both disabled, so they
// must be enabled per spawn; ordinary user tokens may not carry them
// at all, in which case the returned error lets the runner fail
// closed instead of degrading to an unsandboxed spawn.
func enableCreateProcessAsUserPrivileges() error {
	for _, name := range []string{"SeIncreaseQuotaPrivilege", "SeAssignPrimaryTokenPrivilege"} {
		if err := enablePrivilege(name); err != nil {
			return err
		}
	}
	return nil
}

// enablePrivilege enables one named privilege on the current process
// token and verifies the result: AdjustTokenPrivileges reports
// ERROR_NOT_ALL_PRIVILEGES_ASSIGNED via its last-error without failing
// the call, so a missing privilege must be detected by re-querying.
func enablePrivilege(name string) error {
	name16, err := xwin.UTF16PtrFromString(name)
	if err != nil {
		return errdefs.Internal(fmt.Errorf("windows: encode privilege name %s: %w", name, err))
	}
	var luid xwin.LUID
	if err := xwin.LookupPrivilegeValue(nil, name16, &luid); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: lookup privilege %s: %w", name, err))
	}
	var token xwin.Token
	if err := xwin.OpenProcessToken(xwin.CurrentProcess(),
		xwin.TOKEN_ADJUST_PRIVILEGES|xwin.TOKEN_QUERY, &token); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: open token for privileges: %w", err))
	}
	defer func() { _ = token.Close() }()
	tp := xwin.Tokenprivileges{
		PrivilegeCount: 1,
		Privileges: [1]xwin.LUIDAndAttributes{{
			Luid:       luid,
			Attributes: xwin.SE_PRIVILEGE_ENABLED,
		}},
	}
	if err := xwin.AdjustTokenPrivileges(token, false, &tp, 0, nil, nil); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: enable privilege %s: %w", name, err))
	}
	enabled, err := privilegeEnabled(token, luid)
	if err != nil {
		return err
	}
	if !enabled {
		return errdefs.NotAvailablef(
			"windows: privilege %s is not present in the host token; write confinement requires an elevated host",
			name)
	}
	return nil
}

// privilegeEnabled reports whether luid is enabled in token.
func privilegeEnabled(token xwin.Token, luid xwin.LUID) (bool, error) {
	var size uint32
	if err := xwin.GetTokenInformation(token, xwin.TokenPrivileges, nil, 0, &size); err != nil {
		if !errors.Is(err, xwin.ERROR_INSUFFICIENT_BUFFER) {
			return false, errdefs.Internal(fmt.Errorf("windows: query privilege size: %w", err))
		}
	}
	if size == 0 || size > 64*1024 {
		return false, errdefs.Internal(fmt.Errorf("windows: unexpected privilege buffer size %d", size))
	}
	buf := make([]byte, size)
	if err := xwin.GetTokenInformation(token, xwin.TokenPrivileges, &buf[0], size, &size); err != nil {
		return false, errdefs.Internal(fmt.Errorf("windows: query privileges: %w", err))
	}
	tp := (*xwin.Tokenprivileges)(unsafe.Pointer(&buf[0]))
	for _, p := range tp.AllPrivileges() {
		if p.Luid == luid {
			return p.Attributes&xwin.SE_PRIVILEGE_ENABLED != 0, nil
		}
	}
	return false, nil
}
