//go:build windows

package windows

import (
	"fmt"
	"runtime"
	"unsafe"

	"github.com/GizClaw/flowcraft/core/errdefs"
	xwin "golang.org/x/sys/windows"
)

// AppContainer API declarations. x/sys/windows does not expose the
// userenv.dll AppContainer surface or the advapi32 DACL builders, so
// they are declared here with LazyDLL, same as the write-confinement
// helpers in syscall_windows.go.
var (
	procCreateAppContainerProfile = xwin.NewLazySystemDLL("userenv.dll").NewProc("CreateAppContainerProfile")
	procDeleteAppContainerProfile = xwin.NewLazySystemDLL("userenv.dll").NewProc("DeleteAppContainerProfile")
	// CreateAppContainerToken is a SecurityBaseApi function exported
	// from kernelbase (not userenv, despite the profile functions
	// living there; kernel32 does not forward it either).
	procCreateAppContainerToken   = xwin.NewLazySystemDLL("kernelbase.dll").NewProc("CreateAppContainerToken")
	procDeriveCapabilitySids      = xwin.NewLazySystemDLL("kernel32.dll").NewProc("DeriveCapabilitySidsFromName")
	procGetSecurityDescriptorDacl = xwin.NewLazySystemDLL("advapi32.dll").NewProc("GetSecurityDescriptorDacl")
	// SetEntriesInAcl is a header macro; the real export is the
	// Unicode variant.
	procSetEntriesInAcl = xwin.NewLazySystemDLL("advapi32.dll").NewProc("SetEntriesInAclW")
)

// createAppContainerProfile creates a new AppContainer profile with
// the given capabilities (nil for none) and returns its package SID.
// Creating a profile requires an elevated host; without it the call
// fails closed.
func createAppContainerProfile(name, displayName string, caps []*xwin.SID) (*xwin.SID, error) {
	namePtr, err := xwin.UTF16PtrFromString(name)
	if err != nil {
		return nil, errdefs.Internal(fmt.Errorf("windows: encode appcontainer name: %w", err))
	}
	displayPtr, err := xwin.UTF16PtrFromString(displayName)
	if err != nil {
		return nil, errdefs.Internal(fmt.Errorf("windows: encode appcontainer display name: %w", err))
	}
	descPtr, err := xwin.UTF16PtrFromString("flowcraft sandbox network isolation")
	if err != nil {
		return nil, errdefs.Internal(fmt.Errorf("windows: encode appcontainer description: %w", err))
	}
	var capsPtr **xwin.SID
	if len(caps) > 0 {
		capsPtr = &caps[0]
	}
	var sid *xwin.SID
	r1, _, e1 := procCreateAppContainerProfile.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(displayPtr)),
		uintptr(unsafe.Pointer(descPtr)),
		uintptr(unsafe.Pointer(capsPtr)),
		uintptr(len(caps)),
		uintptr(unsafe.Pointer(&sid)),
	)
	if r1 != 0 {
		// HRESULT_FROM_WIN32(ERROR_ACCESS_DENIED): creating profiles
		// requires an elevated host. Fail closed with NotAvailable so
		// callers get an actionable message instead of a raw HRESULT.
		if r1 == 0x80070005 {
			return nil, errdefs.NotAvailablef(
				"windows: create appcontainer profile %s: requires an elevated host (0x80070005)", name)
		}
		return nil, errdefs.Internal(fmt.Errorf(
			"windows: create appcontainer profile %s: 0x%x (%v)", name, r1, e1))
	}
	return sid, nil
}

// deleteAppContainerProfile removes the profile. A missing profile is
// a successful no-op so cleanup is idempotent.
func deleteAppContainerProfile(name string) error {
	namePtr, err := xwin.UTF16PtrFromString(name)
	if err != nil {
		return errdefs.Internal(fmt.Errorf("windows: encode appcontainer name: %w", err))
	}
	r1, _, _ := procDeleteAppContainerProfile.Call(uintptr(unsafe.Pointer(namePtr)))
	if r1 != 0 {
		return errdefs.Internal(fmt.Errorf(
			"windows: delete appcontainer profile %s: 0x%x", name, r1))
	}
	return nil
}

// sidAndAttributes mirrors SID_AND_ATTRIBUTES for the capabilities
// array inside SECURITY_CAPABILITIES.
type sidAndAttributes struct {
	Sid        *xwin.SID
	Attributes uint32
}

// securityCapabilities mirrors SECURITY_CAPABILITIES, which
// CreateAppContainerToken expects as its second parameter (the
// package SID plus optional capability SIDs, not a bare SID).
type securityCapabilities struct {
	AppContainerSid *xwin.SID
	Capabilities    *sidAndAttributes
	CapabilityCount uint32
	Reserved        uint32
}

// createAppContainerToken derives an AppContainer (lowbox) token from
// an existing token. The resulting token carries the package SID as
// its user — which is what WFP scopes network filters on — plus the
// named capabilities (e.g. InternetClient for allow_list / proxy
// modes). The export lives in kernelbase and follows BOOL semantics
// (nonzero success), matching Chromium's production usage.
func createAppContainerToken(base xwin.Token, sid *xwin.SID, caps []*xwin.SID) (xwin.Token, error) {
	sc := securityCapabilities{AppContainerSid: sid}
	var attrs []sidAndAttributes
	if len(caps) > 0 {
		attrs = make([]sidAndAttributes, len(caps))
		for i, c := range caps {
			attrs[i] = sidAndAttributes{Sid: c, Attributes: xwin.SE_GROUP_ENABLED}
		}
		sc.Capabilities = &attrs[0]
		sc.CapabilityCount = uint32(len(caps))
	}
	var lowbox xwin.Token
	r1, _, e1 := procCreateAppContainerToken.Call(
		uintptr(base),
		uintptr(unsafe.Pointer(&sc)),
		uintptr(unsafe.Pointer(&lowbox)),
	)
	if r1 == 0 {
		return 0, errdefs.Internal(fmt.Errorf(
			"windows: create appcontainer token: 0x%x (%v)", r1, e1))
	}
	return lowbox, nil
}

// capabilitySids resolves a named capability (e.g. "internetClient")
// into its SID, suitable for CreateAppContainerProfile. The returned
// SIDs are freshly allocated and must be freed with xwin.LocalFree.
func capabilitySids(name string) ([]*xwin.SID, error) {
	namePtr, err := xwin.UTF16PtrFromString(name)
	if err != nil {
		return nil, errdefs.Internal(fmt.Errorf("windows: encode capability name: %w", err))
	}
	var groupSids, capSids *xwin.SID
	var count uint32
	r1, _, e1 := procDeriveCapabilitySids.Call(
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(&groupSids)),
		uintptr(unsafe.Pointer(&capSids)),
		uintptr(unsafe.Pointer(&count)),
	)
	if r1 == 0 {
		return nil, errdefs.Internal(fmt.Errorf(
			"windows: derive capability sids %q: %v", name, e1))
	}
	defer func() {
		if groupSids != nil {
			_, _ = xwin.LocalFree(xwin.Handle(unsafe.Pointer(groupSids)))
		}
		if capSids != nil {
			_, _ = xwin.LocalFree(xwin.Handle(unsafe.Pointer(capSids)))
		}
	}()
	sids := make([]*xwin.SID, int(count))
	for i := range sids {
		sids[i] = *(**xwin.SID)(unsafe.Pointer(
			uintptr(unsafe.Pointer(capSids)) + uintptr(i)*unsafe.Sizeof(uintptr(0))))
	}
	return sids, nil
}

// grantDaclAccess adds a GRANT_ACCESS ACE for sid to path's DACL,
// preserving every existing entry. Directories pass inherit flags so
// future children carry the grant.
func grantDaclAccess(path string, sid *xwin.SID, access uint32, inherit uint32) error {
	sd, err := xwin.GetNamedSecurityInfo(path, xwin.SE_FILE_OBJECT, xwin.DACL_SECURITY_INFORMATION)
	if err != nil {
		return errdefs.Internal(fmt.Errorf("windows: get dacl of %s: %w", path, err))
	}
	var present, defaulted bool
	var dacl *xwin.ACL
	r1, _, e1 := procGetSecurityDescriptorDacl.Call(
		uintptr(unsafe.Pointer(sd)),
		uintptr(unsafe.Pointer(&present)),
		uintptr(unsafe.Pointer(&dacl)),
		uintptr(unsafe.Pointer(&defaulted)),
	)
	if r1 == 0 {
		return errdefs.Internal(fmt.Errorf("windows: read dacl of %s: %v", path, e1))
	}

	// The OBJECTS_AND_SID is referenced from TRUSTEE.TrusteeValue,
	// which is a uintptr and invisible to the GC, so it must be
	// pinned for the duration of the SetEntriesInAclW call (see the
	// pinning contract on x/sys TrusteeValueFromObjectsAndSid).
	oas := &xwin.OBJECTS_AND_SID{Sid: sid}
	var pinner runtime.Pinner
	pinner.Pin(oas)
	defer pinner.Unpin()

	ea := xwin.EXPLICIT_ACCESS{
		AccessPermissions: xwin.ACCESS_MASK(access),
		AccessMode:        xwin.GRANT_ACCESS,
		Inheritance:       inherit,
		Trustee: xwin.TRUSTEE{
			TrusteeForm:  xwin.TRUSTEE_IS_SID,
			TrusteeType:  xwin.TRUSTEE_IS_UNKNOWN,
			TrusteeValue: xwin.TrusteeValueFromObjectsAndSid(oas),
		},
	}
	var newDacl *xwin.ACL
	r1, _, e2 := procSetEntriesInAcl.Call(
		1,
		uintptr(unsafe.Pointer(&ea)),
		uintptr(unsafe.Pointer(dacl)),
		uintptr(unsafe.Pointer(&newDacl)),
	)
	// SetEntriesInAclW returns a Win32 error code in r1 (RAX): zero
	// means success. r2 is a scratch register, not the return value.
	if r1 != 0 {
		return errdefs.Internal(fmt.Errorf("windows: merge dacl of %s: %v", path, e2))
	}
	defer func() { _, _ = xwin.LocalFree(xwin.Handle(unsafe.Pointer(newDacl))) }()

	if err := xwin.SetNamedSecurityInfo(path, xwin.SE_FILE_OBJECT,
		xwin.DACL_SECURITY_INFORMATION, nil, nil, newDacl, nil); err != nil {
		return errdefs.Internal(fmt.Errorf("windows: apply dacl to %s: %w", path, err))
	}
	return nil
}
