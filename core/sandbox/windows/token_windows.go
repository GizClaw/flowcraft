//go:build windows

package windows

import (
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
