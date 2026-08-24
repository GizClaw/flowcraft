//go:build windows

package windows

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The elevated setup creates two dedicated local accounts whose SIDs
// anchor WFP network filters and workspace ACLs (mirroring codex-rs's
// CodexSandboxOffline / CodexSandboxOnline split):
//
//   - offline: carries persistent catch-all WFP block filters, used
//     for every network-enforced mode (NetDenyAll / NetAllowList /
//     NetProxy);
//   - online: no filters, used for NetDefault.
//
// The unelevated backend cannot CreateProcessAsUser to either account,
// so an elevated helper holds the credentials and spawns on its
// behalf.
const (
	sandboxAccountOffline = "FlowCraftSandboxOffline"
	sandboxAccountOnline  = "FlowCraftSandboxOnline"
	setupVersion          = 2

	// NetUserAdd / NetUserSetInfo return codes.
	nerrSuccess    = 0
	nerrUserExists = 2224

	// USER_INFO_1 flags / privilege.
	ufScript           = 0x00000001
	ufPasswdCantChange = 0x00000040
	ufDontExpirePasswd = 0x00010000
	usPrivUser         = 1

	logon32LogonNetwork    = 3
	logon32ProviderDefault = 0

	// SEE_MASK_NOCLOSEPROCESS for ShellExecuteExW.
	seeMaskNoCloseProcess = 0x40
)

var (
	modnetapi32        = windows.NewLazySystemDLL("netapi32.dll")
	procNetUserAdd     = modnetapi32.NewProc("NetUserAdd")
	procNetUserSetInfo = modnetapi32.NewProc("NetUserSetInfo")

	modshell32          = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteExW = modshell32.NewProc("ShellExecuteExW")

	procLogonUserW = modadvapi32.NewProc("LogonUserW")
)

// userInfo1 mirrors USER_INFO_1 for NetUserAdd.
type userInfo1 struct {
	name        *uint16
	password    *uint16
	passwordAge uint32
	priv        uint32
	homeDir     *uint16
	comment     *uint16
	flags       uint32
	scriptPath  *uint16
}

// userInfo1003 mirrors USER_INFO_1003 for NetUserSetInfo (password).
type userInfo1003 struct {
	password *uint16
}

// accountCred is one persisted account credential. Password is stored
// DPAPI-encrypted (base64), so only the same Windows user can read it.
type accountCred struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// sandboxCreds holds both sandbox accounts.
type sandboxCreds struct {
	Offline *accountCred `json:"offline"`
	Online  *accountCred `json:"online"`
}

// setupMarker records a completed elevated install.
type setupMarker struct {
	Version      int       `json:"version"`
	WFPInstalled bool      `json:"wfp_installed"`
	InstalledAt  time.Time `json:"installed_at"`
}

// SandboxHelperInstall creates both sandbox accounts (idempotent),
// installs the WFP filters for the offline account, and writes the
// DPAPI-protected credentials plus the setup marker. It must run
// elevated; the runner only calls it through a runas re-exec launch.
func SandboxHelperInstall(configDir string) error {
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("windows/elevated: create config dir: %w", err)
	}
	offlinePass, err := randomPassword()
	if err != nil {
		return err
	}
	onlinePass, err := randomPassword()
	if err != nil {
		return err
	}
	for _, acct := range []struct {
		username string
		password string
	}{
		{sandboxAccountOffline, offlinePass},
		{sandboxAccountOnline, onlinePass},
	} {
		switch err := netUserAdd(acct.username, acct.password); {
		case err == nil:
			// Created.
		case errors.Is(err, errUserExists):
			// Reuse the account; refresh its password so old tokens are
			// invalidated and the stored credential matches.
			if err := netUserSetPassword(acct.username, acct.password); err != nil {
				return err
			}
		default:
			return fmt.Errorf("windows/elevated: create sandbox account %q: %w", acct.username, err)
		}
	}
	creds := &sandboxCreds{
		Offline: &accountCred{Username: sandboxAccountOffline, Password: offlinePass},
		Online:  &accountCred{Username: sandboxAccountOnline, Password: onlinePass},
	}
	if err := saveCreds(configDir, creds); err != nil {
		return err
	}
	if _, err := installWFPFilters(sandboxAccountOffline); err != nil {
		return fmt.Errorf("windows/elevated: install wfp filters: %w", err)
	}
	return writeMarker(configDir)
}

var errUserExists = errors.New("windows/elevated: sandbox account already exists")

func netUserAdd(username, password string) error {
	u, err := windows.UTF16PtrFromString(username)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	info := userInfo1{
		name:     u,
		password: p,
		priv:     usPrivUser,
		flags:    ufScript | ufPasswdCantChange | ufDontExpirePasswd,
	}
	var parmErr uint32
	r1, _, _ := procNetUserAdd.Call(
		0,
		1,
		uintptr(unsafe.Pointer(&info)),
		uintptr(unsafe.Pointer(&parmErr)),
	)
	switch r1 {
	case nerrSuccess:
		return nil
	case nerrUserExists:
		return errUserExists
	default:
		return fmt.Errorf("NetUserAdd failed with 0x%x (parameter %d)", r1, parmErr)
	}
}

func netUserSetPassword(username, password string) error {
	u, err := windows.UTF16PtrFromString(username)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return err
	}
	info := userInfo1003{password: p}
	r1, _, _ := procNetUserSetInfo.Call(
		0,
		uintptr(unsafe.Pointer(u)),
		1003,
		uintptr(unsafe.Pointer(&info)),
		0,
	)
	if r1 != nerrSuccess {
		return fmt.Errorf("NetUserSetInfo failed with 0x%x", r1)
	}
	return nil
}

func randomPassword() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("windows/elevated: generate password: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// saveCreds persists the credentials with passwords DPAPI-protected.
func saveCreds(dir string, creds *sandboxCreds) error {
	for _, acct := range []*accountCred{creds.Offline, creds.Online} {
		blob, err := dpapiProtect([]byte(acct.Password))
		if err != nil {
			return fmt.Errorf("windows/elevated: protect credential: %w", err)
		}
		acct.Password = base64.StdEncoding.EncodeToString(blob)
	}
	buf, err := json.Marshal(creds)
	if err != nil {
		return err
	}
	return os.WriteFile(credsPath(dir), buf, 0o600)
}

// loadCreds reads and decrypts the persisted credential.
func loadCreds(dir string) (*sandboxCreds, error) {
	buf, err := os.ReadFile(credsPath(dir))
	if err != nil {
		return nil, err
	}
	var creds sandboxCreds
	if err := json.Unmarshal(buf, &creds); err != nil {
		return nil, fmt.Errorf("windows/elevated: decode credentials: %w", err)
	}
	for _, acct := range []*accountCred{creds.Offline, creds.Online} {
		if acct == nil {
			return nil, fmt.Errorf("windows/elevated: credentials file missing an account")
		}
		blob, err := base64.StdEncoding.DecodeString(acct.Password)
		if err != nil {
			return nil, fmt.Errorf("windows/elevated: decode credential blob: %w", err)
		}
		plain, err := dpapiUnprotect(blob)
		if err != nil {
			return nil, fmt.Errorf("windows/elevated: unprotect credential: %w", err)
		}
		acct.Password = string(plain)
	}
	return &creds, nil
}

func writeMarker(dir string) error {
	m := setupMarker{Version: setupVersion, WFPInstalled: true, InstalledAt: time.Now()}
	buf, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(markerPath(dir), buf, 0o600)
}

// setupComplete reports whether the accounts + credentials + WFP
// filters exist and match the current setup version.
func setupComplete(dir string) bool {
	buf, err := os.ReadFile(markerPath(dir))
	if err != nil {
		return false
	}
	var m setupMarker
	if json.Unmarshal(buf, &m) != nil || m.Version != setupVersion || !m.WFPInstalled {
		return false
	}
	_, err = loadCreds(dir)
	return err == nil
}

// accountSID resolves the sandbox account's SID (needed for workspace
// ACL grants and, in P2b, WFP user conditions).
func accountSID(username string) (*windows.SID, error) {
	u, err := windows.UTF16PtrFromString(username)
	if err != nil {
		return nil, err
	}
	var sidLen uint32
	var domLen uint32
	var use uint32
	err = windows.LookupAccountName(nil, u, nil, &sidLen, nil, &domLen, &use)
	if err != nil && !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		return nil, fmt.Errorf("windows/elevated: size lookup for %q: %w", username, err)
	}
	sidBuf := make([]byte, sidLen)
	var domPtr *uint16
	if domLen > 0 {
		domBuf := make([]uint16, domLen)
		domPtr = &domBuf[0]
	}
	if err := windows.LookupAccountName(
		nil,
		u,
		(*windows.SID)(unsafe.Pointer(&sidBuf[0])),
		&sidLen,
		domPtr,
		&domLen,
		&use,
	); err != nil {
		return nil, fmt.Errorf("windows/elevated: lookup %q: %w", username, err)
	}
	return (*windows.SID)(unsafe.Pointer(&sidBuf[0])).Copy()
}

// logonSandboxUser builds a primary token for one sandbox account via
// LogonUserW (NETWORK logon), used by the elevated runner to spawn.
func logonSandboxUser(acct *accountCred) (windows.Token, error) {
	u, err := windows.UTF16PtrFromString(acct.Username)
	if err != nil {
		return 0, err
	}
	p, err := windows.UTF16PtrFromString(acct.Password)
	if err != nil {
		return 0, err
	}
	var tok windows.Token
	r1, _, e1 := procLogonUserW.Call(
		0,
		0,
		uintptr(unsafe.Pointer(u)),
		uintptr(unsafe.Pointer(p)),
		logon32LogonNetwork,
		logon32ProviderDefault,
		uintptr(unsafe.Pointer(&tok)),
	)
	if r1 == 0 {
		return 0, fmt.Errorf("windows/elevated: logon sandbox user: %w", e1)
	}
	return tok, nil
}

// shellExecuteInfoW mirrors SHELLEXECUTEINFOW (x64 layout).
type shellExecuteInfoW struct {
	cbSize       uint32
	fMask        uint32
	hwnd         uintptr
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	_            uint32
	hInstApp     uintptr
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    uintptr
	dwHotKey     uint32
	_            uint32
	hIcon        uintptr
	hProcess     uintptr
}

// launchElevated starts exePath with args through ShellExecuteExW's
// "runas" verb (UAC prompt), hiding the helper window. When wait is
// true it blocks until the helper exits and returns its exit code.
func launchElevated(exePath, args string, wait bool) (uint32, error) {
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return 0, err
	}
	file, err := windows.UTF16PtrFromString(exePath)
	if err != nil {
		return 0, err
	}
	params, err := windows.UTF16PtrFromString(args)
	if err != nil {
		return 0, err
	}
	var sei shellExecuteInfoW
	sei.cbSize = uint32(unsafe.Sizeof(sei))
	sei.fMask = seeMaskNoCloseProcess
	sei.lpVerb = verb
	sei.lpFile = file
	sei.lpParameters = params
	sei.nShow = 0 // SW_HIDE
	r1, _, e1 := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&sei)))
	if r1 == 0 {
		return 0, fmt.Errorf("windows/elevated: launch helper elevated: %w", e1)
	}
	if sei.hProcess == 0 {
		return 0, nil
	}
	if !wait {
		// The helper runs independently; release the process handle so
		// the runner does not leak one per launch.
		_ = windows.CloseHandle(windows.Handle(sei.hProcess))
		return 0, nil
	}
	if _, err := windows.WaitForSingleObject(windows.Handle(sei.hProcess), windows.INFINITE); err != nil {
		_ = windows.CloseHandle(windows.Handle(sei.hProcess))
		return 0, err
	}
	var code uint32
	_ = windows.GetExitCodeProcess(windows.Handle(sei.hProcess), &code)
	_ = windows.CloseHandle(windows.Handle(sei.hProcess))
	return code, nil
}

func dpapiProtect(data []byte) ([]byte, error) {
	var in windows.DataBlob
	if len(data) > 0 {
		in.Data = &data[0]
		in.Size = uint32(len(data))
	}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return unsafe.Slice(out.Data, out.Size), nil
}

func dpapiUnprotect(blob []byte) ([]byte, error) {
	var in windows.DataBlob
	if len(blob) > 0 {
		in.Data = &blob[0]
		in.Size = uint32(len(blob))
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return unsafe.Slice(out.Data, out.Size), nil
}
