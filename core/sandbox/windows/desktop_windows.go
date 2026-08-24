//go:build windows

package windows

import (
	"crypto/rand"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// desktopAllAccess is DESKTOP_ALL_ACCESS (winuser.h): the union of
// every DESKTOP_* right. GENERIC_ALL on the DACL maps onto it.
const desktopAllAccess = 0x000F01FF

var (
	modUser32          = windows.NewLazySystemDLL("user32.dll")
	procCreateDesktopW = modUser32.NewProc("CreateDesktopW")
	procCloseDesktop   = modUser32.NewProc("CloseDesktop")
)

// createSandboxDesktop creates a private desktop under the caller's
// window station, ACL'd for the sandbox account, and returns its name
// and an open handle. Console children of the elevated backend need a
// desktop they can actually access: the service session's default
// desktop denies plain users, and cmd.exe's console initialization
// hangs there with no output. Pass the name as
// STARTUPINFO.lpDesktop (bare name = the caller's window station).
// The caller must close the handle with closeSandboxDesktop once the
// child has finished.
func createSandboxDesktop(accountSID string) (string, windows.Handle, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return "", 0, fmt.Errorf("windows/elevated: desktop random: %w", err)
	}
	name := fmt.Sprintf("FlowcraftSandboxDesktop-%x", rnd)
	sddl := fmt.Sprintf("D:(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;%s)", accountSID)
	sd, free, err := securityDescriptorFromSDDL(sddl)
	if err != nil {
		return "", 0, err
	}
	defer free()
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
		InheritHandle:      0,
	}
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", 0, err
	}
	r1, _, e1 := procCreateDesktopW.Call(
		uintptr(unsafe.Pointer(namePtr)),
		0,
		0,
		0,
		uintptr(desktopAllAccess),
		uintptr(unsafe.Pointer(sa)),
	)
	if r1 == 0 {
		return "", 0, fmt.Errorf("windows/elevated: create desktop %q: %w", name, e1)
	}
	return name, windows.Handle(r1), nil
}

func closeSandboxDesktop(h windows.Handle) {
	if h == 0 {
		return
	}
	_, _, _ = procCloseDesktop.Call(uintptr(h))
}

// securityDescriptorFromSDDL builds a self-relative security
// descriptor from an SDDL string. The caller must run the returned
// free function (LocalFree) when done.
func securityDescriptorFromSDDL(sddl string) (*windows.SECURITY_DESCRIPTOR, func(), error) {
	sddlPtr, err := windows.UTF16PtrFromString(sddl)
	if err != nil {
		return nil, nil, err
	}
	var sd uintptr
	var size uint32
	r1, _, e1 := procConvertStringSecurityDescriptorToSecurityDescriptorW.Call(
		uintptr(unsafe.Pointer(sddlPtr)),
		1, // SECURITY_DESCRIPTOR_REVISION
		uintptr(unsafe.Pointer(&sd)),
		uintptr(unsafe.Pointer(&size)),
	)
	if r1 == 0 {
		return nil, nil, fmt.Errorf("windows/elevated: build security descriptor: %w", e1)
	}
	sdPtr := *(*unsafe.Pointer)(unsafe.Pointer(&sd))
	return (*windows.SECURITY_DESCRIPTOR)(sdPtr), func() {
		if sd != 0 {
			_, _ = windows.LocalFree(windows.Handle(sd))
		}
	}, nil
}
