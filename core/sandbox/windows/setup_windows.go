//go:build windows

package windows

import (
	"crypto/rand"
	"encoding/base64"
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
	// Local account names are capped at 20 characters by NetUserAdd
	// (SAM account-name limit); longer names fail with
	// ERROR_BAD_USERNAME (0x89a).
	sandboxAccountOffline = "FlowCraftSbxOffline"
	sandboxAccountOnline  = "FlowCraftSbxOnline"
	// Bumped from 2: the sandbox account names changed, so machines
	// that ran the old setup must re-create accounts and filters.
	setupVersion = 3

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
	const (
		lower   = "abcdefghijklmnopqrstuvwxyz"
		upper   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		digits  = "0123456789"
		symbols = "!@#$%^&*()-_=+[]{};:,.<>?"
	)
	all := lower + upper + digits + symbols
	classes := []string{lower, upper, digits, symbols}
	b := make([]byte, 24)
	for i := range b {
		var r [1]byte
		if _, err := rand.Read(r[:]); err != nil {
			return "", fmt.Errorf("windows/elevated: generate password: %w", err)
		}
		b[i] = all[int(r[0])%len(all)]
	}
	// Force one character from every class so common password
	// complexity policies (three of four classes) cannot reject the
	// password; a hex-only draw fails NetUserAdd with
	// NERR_PasswordTooShort on hardened machines.
	for i, class := range classes {
		var r [1]byte
		if _, err := rand.Read(r[:]); err != nil {
			return "", fmt.Errorf("windows/elevated: generate password: %w", err)
		}
		b[i] = class[int(r[0])%len(class)]
	}
	// Shuffle so the forced class positions are not predictable.
	for i := len(b) - 1; i > 0; i-- {
		var r [1]byte
		if _, err := rand.Read(r[:]); err != nil {
			return "", fmt.Errorf("windows/elevated: generate password: %w", err)
		}
		j := int(r[0]) % (i + 1)
		b[i], b[j] = b[j], b[i]
	}
	return string(b), nil
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
// configDir receives step diagnostics so a silent death during the
// call is bisectable from the helper log.
func logonSandboxUser(acct *accountCred, configDir string) (windows.Token, error) {
	u, err := windows.UTF16PtrFromString(acct.Username)
	if err != nil {
		return 0, err
	}
	// "." pins the local machine explicitly. A NULL domain makes
	// LogonUserW take the UPN/network-account lookup path, which
	// access-violates (read of address 0x0) in the CI service
	// session; codex-rs passes "." for the same reason.
	domain, err := windows.UTF16PtrFromString(".")
	if err != nil {
		return 0, err
	}
	p, err := windows.UTF16PtrFromString(acct.Password)
	if err != nil {
		return 0, err
	}
	var tok windows.Token
	appendHelperLog(configDir, fmt.Errorf("logon: calling LogonUserW for %q type=%d", acct.Username, logon32LogonNetwork))
	r1, _, e1 := procLogonUserW.Call(
		uintptr(unsafe.Pointer(u)),
		uintptr(unsafe.Pointer(domain)),
		uintptr(unsafe.Pointer(p)),
		logon32LogonNetwork,
		logon32ProviderDefault,
		uintptr(unsafe.Pointer(&tok)),
	)
	appendHelperLog(configDir, fmt.Errorf("logon: LogonUserW returned r1=%d err=%v", r1, e1))
	if r1 == 0 {
		return 0, fmt.Errorf("windows/elevated: logon sandbox user: %w", e1)
	}
	return tok, nil
}

// logTokenPrivileges records which privileges the helper's token
// holds, so an elevated-path failure can be checked against the
// process' actual rights (e.g. SeIncreaseQuotaPrivilege is required
// by CreateProcessAsUser and is rarely present on CI runners).
func logTokenPrivileges(configDir string) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		appendHelperLog(configDir, fmt.Errorf("open token for privilege check: %v", err))
		return
	}
	defer func() { _ = tok.Close() }()
	var needed uint32
	_ = windows.GetTokenInformation(tok, windows.TokenPrivileges, nil, 0, &needed)
	buf := make([]byte, needed)
	if err := windows.GetTokenInformation(tok, windows.TokenPrivileges, &buf[0], needed, &needed); err != nil {
		appendHelperLog(configDir, fmt.Errorf("query token privileges: %v", err))
		return
	}
	privs := (*windows.Tokenprivileges)(unsafe.Pointer(&buf[0]))
	// The rights that matter for the elevated spawn path.
	interesting := []string{
		"SeIncreaseQuotaPrivilege",
		"SeAssignPrimaryTokenPrivilege",
		"SeTcbPrivilege",
		"SeDebugPrivilege",
		"SeBackupPrivilege",
		"SeRestorePrivilege",
	}
	var found []string
	for _, name := range interesting {
		var want windows.LUID
		u, err := windows.UTF16PtrFromString(name)
		if err != nil {
			continue
		}
		if err := windows.LookupPrivilegeValue(nil, u, &want); err != nil {
			continue
		}
		for _, p := range privs.AllPrivileges() {
			if p.Luid == want {
				state := "present"
				if p.Attributes&windows.SE_PRIVILEGE_ENABLED != 0 {
					state = "enabled"
				}
				found = append(found, name+"="+state)
			}
		}
	}
	appendHelperLog(configDir, fmt.Errorf("helper privileges: %v (total %d)", found, privs.PrivilegeCount))
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

// launchElevated starts exePath with args. When the current process
// is already elevated it spawns directly via CreateProcess (no UAC
// prompt) and redirects stderr to errLog; otherwise it falls back to
// ShellExecuteExW's "runas" verb (UAC prompt), hiding the helper
// window. When wait is true it blocks until the helper exits and
// returns its exit code.
func launchElevated(exePath, args string, wait bool, errLog string) (uint32, error) {
	if elevated, err := currentProcessElevated(); err == nil && elevated {
		return launchElevatedDirect(exePath, args, wait, errLog)
	}
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

// currentProcessElevated reports whether the calling process runs
// with a full (elevated) token, in which case the helper needs no
// runas prompt and can be spawned directly.
func currentProcessElevated() (bool, error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		return false, err
	}
	defer func() { _ = tok.Close() }()
	return tok.IsElevated(), nil
}

// launchElevatedDirect spawns exePath with args under the caller's
// already-elevated token, redirecting stderr to errLog so a hidden
// native fatal is not invisible. The helper window is suppressed.
func launchElevatedDirect(exePath, args string, wait bool, errLog string) (uint32, error) {
	cmdline, err := windows.UTF16PtrFromString(quoteCommandLine(exePath, args))
	if err != nil {
		return 0, err
	}
	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Flags = windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW
	si.ShowWindow = windows.SW_HIDE
	var errFile *os.File
	if errLog != "" {
		errFile, err = os.OpenFile(errLog, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, err
		}
		defer func() { _ = errFile.Close() }()
		si.StdErr = windows.Handle(errFile.Fd())
	}
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(
		nil,
		cmdline,
		nil,
		nil,
		true,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW,
		nil,
		nil,
		&si,
		&pi,
	); err != nil {
		return 0, fmt.Errorf("windows/elevated: launch helper: %w", err)
	}
	_ = windows.CloseHandle(pi.Thread)
	if !wait {
		// The helper runs independently; release the process handle so
		// the runner does not leak one per launch.
		_ = windows.CloseHandle(pi.Process)
		return 0, nil
	}
	defer func() { _ = windows.CloseHandle(pi.Process) }()
	if _, err := windows.WaitForSingleObject(pi.Process, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	_ = windows.GetExitCodeProcess(pi.Process, &code)
	return code, nil
}

// quoteCommandLine renders a CreateProcess command line from an
// executable and a pre-quoted argument string.
func quoteCommandLine(exePath, args string) string {
	return quoteArg(exePath) + " " + args
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
	// Copy out of the LocalAlloc'd blob before LocalFree: the deferred
	// free runs after the return expression, so a slice that aliases
	// out.Data would be dangling by the time the caller reads it.
	plain := make([]byte, out.Size)
	copy(plain, unsafe.Slice(out.Data, out.Size))
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return plain, nil
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
	plain := make([]byte, out.Size)
	copy(plain, unsafe.Slice(out.Data, out.Size))
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return plain, nil
}
