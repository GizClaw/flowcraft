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
// a restricted version of the caller's own primary token (every
// privilege disabled) whose integrity level is lowered to Low. The
// restricted token is derived directly from the caller's primary token
// so CreateProcessAsUser's restricted-token exemption applies (no
// SE_ASSIGNPRIMARYTOKEN_NAME required), letting ordinary applications
// create restricted processes.
func restrictToken() (xwin.Token, error) {
	var current xwin.Token
	if err := xwin.OpenProcessToken(xwin.CurrentProcess(), xwin.TOKEN_ALL_ACCESS, &current); err != nil {
		return 0, errdefs.Internal(fmt.Errorf("windows: open current token: %w", err))
	}
	defer func() { _ = current.Close() }()

	restricted, err := createRestrictedToken(current)
	if err != nil {
		return 0, errdefs.Internal(fmt.Errorf("windows: restrict token: %w", err))
	}

	// The restricted handle mirrors the source's access rights
	// (TOKEN_ALL_ACCESS), so lowering its integrity level afterwards is
	// allowed.
	lowSID, err := lowIntegritySID()
	if err != nil {
		_ = restricted.Close()
		return 0, err
	}
	label := tokenMandatoryLabel{
		Label: xwin.SIDAndAttributes{Sid: lowSID, Attributes: xwin.SE_GROUP_INTEGRITY},
	}
	if err := xwin.SetTokenInformation(restricted, uint32(xwin.TokenIntegrityLevel),
		(*byte)(unsafe.Pointer(&label)), uint32(unsafe.Sizeof(label))); err != nil {
		_ = restricted.Close()
		return 0, errdefs.Internal(fmt.Errorf("windows: set low integrity on token: %w", err))
	}
	return restricted, nil
}

// enableCreateProcessAsUserPrivileges enables SE_INCREASE_QUOTA_NAME,
// which CreateProcessAsUser requires, on the current process token.
// Admin tokens carry it disabled, so it must be enabled per spawn;
// ordinary user tokens may not carry it at all, in which case the
// returned error lets the runner fail closed instead of degrading to
// an unsandboxed spawn.
func enableCreateProcessAsUserPrivileges() error {
	return enablePrivilege("SeIncreaseQuotaPrivilege")
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
