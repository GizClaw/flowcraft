//go:build windows

package windows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"

	otellog "go.opentelemetry.io/otel/log"
	xwin "golang.org/x/sys/windows"
)

// netIsolation is the AppContainer-backed network isolation for one
// runner. The child runs with an AppContainer (lowbox) token whose
// package SID is unique to this runner, so kernel-level network
// restrictions (and, for allow_list / proxy modes, WFP filters) can
// be scoped to exactly the sandboxed process tree.
//
// An AppContainer without network capabilities is denied all network
// access by the OS firewall — that is how NetDenyAll is enforced with
// no WFP involvement. Writable paths are granted to the package SID
// through the filesystem DACL, and HOME / TEMP are redirected into a
// per-runner sandbox directory, because AppContainers cannot read the
// user profile by default.
type netIsolation struct {
	name  string
	sid   *xwin.SID
	token xwin.Token
	home  string
}

// newNetIsolation creates the AppContainer profile, derives the lowbox
// token, and grants the package SID write access to the same path set
// the write-policy would allow. Hosts without the privilege to create
// AppContainer profiles fail closed with NotAvailable.
func newNetIsolation(root string, writable []string, mode corenet.NetMode) (*netIsolation, error) {
	if mode != corenet.NetDenyAll {
		return nil, errdefs.NotAvailablef(
			"windows: net mode %d is not implemented yet", int(mode))
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, errdefs.Internal(fmt.Errorf("windows: net isolation entropy: %w", err))
	}
	name := "flowcraft.sandbox." + hex.EncodeToString(raw[:])

	// No capabilities: the firewall blocks all network for the
	// container. allow_list / proxy modes add InternetClient and WFP
	// filters instead.
	sid, err := createAppContainerProfile(name, "flowcraft sandbox", nil)
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = deleteAppContainerProfile(name) }

	var current xwin.Token
	if err := xwin.OpenProcessToken(xwin.CurrentProcess(), xwin.TOKEN_ALL_ACCESS, &current); err != nil {
		cleanup()
		return nil, errdefs.Internal(fmt.Errorf("windows: open current token: %w", err))
	}
	lowbox, err := createAppContainerToken(current, sid)
	_ = current.Close()
	if err != nil {
		cleanup()
		return nil, errdefs.Internal(fmt.Errorf("windows: net isolation token: %w", err))
	}
	iso := &netIsolation{name: name, sid: sid, token: lowbox}
	if err := iso.setupHome(); err != nil {
		_ = iso.Close()
		return nil, err
	}
	if err := grantPathTree(root, sid, containerWriteAccess); err != nil {
		_ = iso.Close()
		return nil, err
	}
	for _, p := range writable {
		if p == "" || p == root {
			continue
		}
		if err := grantPathTree(p, sid, containerWriteAccess); err != nil {
			_ = iso.Close()
			return nil, err
		}
	}
	return iso, nil
}

// containerWriteAccess is the access a sandboxed child needs inside
// its workspace: read, write, execute, and delete, mirroring what the
// owning user has.
const containerWriteAccess = xwin.FILE_GENERIC_READ |
	xwin.FILE_GENERIC_WRITE | xwin.FILE_GENERIC_EXECUTE | xwin.DELETE

// setupHome creates the per-runner sandbox home (used for HOME, TEMP,
// APPDATA, ...) and grants the package SID write access to it.
func (n *netIsolation) setupHome() error {
	home, err := os.MkdirTemp("", "flowcraft-ac-")
	if err != nil {
		return errdefs.Internal(fmt.Errorf("windows: net isolation home: %w", err))
	}
	for _, sub := range []string{
		"AppData", filepath.Join("AppData", "Roaming"),
		filepath.Join("AppData", "Local"), "Temp",
	} {
		if err := os.MkdirAll(filepath.Join(home, sub), 0o755); err != nil {
			_ = os.RemoveAll(home)
			return errdefs.Internal(fmt.Errorf("windows: net isolation home dirs: %w", err))
		}
	}
	if err := grantPathTree(home, n.sid, containerWriteAccess); err != nil {
		_ = os.RemoveAll(home)
		return err
	}
	n.home = home
	return nil
}

// env redirects the child's environment away from the user profile,
// which an AppContainer cannot read, into the sandbox home. Existing
// keys are replaced case-insensitively (Windows environment is
// case-insensitive).
func (n *netIsolation) env(env []string) []string {
	repl := map[string]string{
		"HOME":         n.home,
		"USERPROFILE":  n.home,
		"APPDATA":      filepath.Join(n.home, "AppData", "Roaming"),
		"LOCALAPPDATA": filepath.Join(n.home, "AppData", "Local"),
		"TEMP":         filepath.Join(n.home, "Temp"),
		"TMP":          filepath.Join(n.home, "Temp"),
		"TMPDIR":       filepath.Join(n.home, "Temp"),
	}
	upper := make(map[string]string, len(repl))
	for k, v := range repl {
		upper[strings.ToUpper(k)] = v
	}
	out := make([]string, 0, len(env)+len(repl))
	seen := make(map[string]bool, len(repl))
	for _, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		if v, ok := upper[strings.ToUpper(key)]; ok {
			out = append(out, key+"="+v)
			seen[strings.ToUpper(key)] = true
			continue
		}
		out = append(out, kv)
	}
	for k, v := range upper {
		if !seen[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// Close removes the AppContainer profile and the sandbox home. It is
// safe to call more than once.
func (n *netIsolation) Close() error {
	var errs []error
	if n.name != "" {
		if err := deleteAppContainerProfile(n.name); err != nil {
			errs = append(errs, err)
		}
		n.name = ""
	}
	if n.token != 0 {
		if err := n.token.Close(); err != nil {
			errs = append(errs, err)
		}
		n.token = 0
	}
	if n.home != "" {
		if err := os.RemoveAll(n.home); err != nil {
			telemetry.WarnErr(context.Background(), "windows: remove net isolation home failed", err,
				otellog.String("windows.isolation_home", n.home))
		}
		n.home = ""
	}
	if len(errs) > 0 {
		return fmt.Errorf("windows: close net isolation: %w", errs[0])
	}
	return nil
}

// grantPathTree adds a DACL grant ACE for sid to path and every
// existing child, recursively. Directories carry the inherit flags so
// new children receive the grant automatically.
func grantPathTree(path string, sid *xwin.SID, access uint32) error {
	return filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var inherit uint32
		if d.IsDir() {
			inherit = xwin.OBJECT_INHERIT_ACE | xwin.CONTAINER_INHERIT_ACE
		}
		return grantDaclAccess(p, sid, access, inherit)
	})
}
