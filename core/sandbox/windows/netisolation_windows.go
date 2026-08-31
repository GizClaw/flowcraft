//go:build windows

package windows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
	"github.com/GizClaw/flowcraft/core/utils/net/mitm"

	otellog "go.opentelemetry.io/otel/log"
	xwin "golang.org/x/sys/windows"
)

// netIsolation is the AppContainer-backed network isolation for one
// runner. The child runs with an AppContainer (lowbox) token whose
// package SID is unique to this runner, so kernel-level network
// restrictions (and, for allow_list / proxy modes, WFP filters) can
// be scoped to exactly the sandboxed process tree.
//
// The container is created without any network capability, so the OS
// firewall's built-in AppIsolation sublayer denies all network access
// by default. That default only covers ALE_AUTH_CONNECT-classified
// flows (TCP and connected UDP); unconnected UDP sendto and ICMP are
// not constrained by it, so every non-default mode also installs WFP
// bind-layer (ALE_RESOURCE_ASSIGNMENT) filters for the package SID.
// NetDenyAll blocks every bind; allow-list / proxy modes permit only
// TCP binds (still pinned to the loopback enforcement proxy at
// ALE_AUTH_CONNECT) and block everything else. Writable paths are
// granted to the package SID through the filesystem DACL, and HOME /
// TEMP are redirected into a per-runner sandbox directory, because
// AppContainers cannot read the user profile by default.
type netIsolation struct {
	name  string
	sid   *xwin.SID
	token xwin.Token
	home  string

	policy    corenet.NetPolicy
	wfp       *wfpIsolation // allow_list / proxy modes
	proxy     *corenet.Proxy
	proxyPort int
	bundle    string // MITM CA bundle staged inside home
}

// newNetIsolation creates the AppContainer profile, derives the lowbox
// token, grants the package SID write access to the same path set the
// write-policy would allow, and (for allow_list / proxy modes) pins
// the container's network to the enforcement proxy with WFP. Hosts
// without the privilege to create AppContainer profiles or open a WFP
// engine fail closed with NotAvailable.
func newNetIsolation(root string, writable []string, policy corenet.NetPolicy) (*netIsolation, error) {
	mode := policy.Mode
	if mode != corenet.NetDenyAll && mode != corenet.NetAllowList && mode != corenet.NetProxy {
		return nil, errdefs.NotAvailablef(
			"windows: net mode %d is not implemented yet", int(mode))
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, errdefs.Internal(fmt.Errorf("windows: net isolation entropy: %w", err))
	}
	name := "flowcraft.sandbox." + hex.EncodeToString(raw[:])

	// No network capabilities in any mode: the AppIsolation default
	// deny covers AUTH_CONNECT-classified flows, and the bind-layer
	// filters below cover everything it misses (UDP/ICMP).
	var caps []*xwin.SID
	sid, err := createAppContainerProfile(name, "flowcraft sandbox", caps)
	if err != nil {
		return nil, err
	}
	cleanup := func() { _ = deleteAppContainerProfile(name) }

	var current xwin.Token
	if err := xwin.OpenProcessToken(xwin.CurrentProcess(), xwin.TOKEN_ALL_ACCESS, &current); err != nil {
		cleanup()
		return nil, errdefs.Internal(fmt.Errorf("windows: open current token: %w", err))
	}
	lowbox, err := createAppContainerToken(current, sid, caps)
	_ = current.Close()
	if err != nil {
		cleanup()
		return nil, errdefs.Internal(fmt.Errorf("windows: net isolation token: %w", err))
	}
	iso := &netIsolation{name: name, sid: sid, token: lowbox, policy: policy}
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
	if mode != corenet.NetDenyAll {
		if err := iso.setupProxy(); err != nil {
			_ = iso.Close()
			return nil, err
		}
	} else if err := iso.setupDenyAll(); err != nil {
		_ = iso.Close()
		return nil, err
	}
	// Behavioral WFP check before the isolation is handed out: filter
	// install success alone does not prove the fence is effective.
	if err := iso.verifyFence(); err != nil {
		_ = iso.Close()
		return nil, err
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

// setupDenyAll closes the AppIsolation coverage gap with a WFP
// bind-layer block: the OS default deny does not constrain unconnected
// UDP or ICMP sockets, so every bind by the package SID is explicitly
// blocked at ALE_RESOURCE_ASSIGNMENT (TCP included — the deny-all
// container needs no sockets at all).
func (n *netIsolation) setupDenyAll() error {
	w, err := newWFPIsolation()
	if err != nil {
		return err
	}
	block := []wfpCondition{sidCondition(n.sid)}
	for _, layer := range []xwin.GUID{
		fwpmLayerAleResourceAssignmentV4, fwpmLayerAleResourceAssignmentV6,
	} {
		if err := w.wfpAddFilter(layer, "flowcraft block sandbox socket bind",
			wfpActionBlock, 0xffffffff, 0, block); err != nil {
			w.Close()
			return err
		}
	}
	n.wfp = w
	return nil
}

// setupProxy starts the host-side enforcement proxy on a loopback
// port and pins the container's network to it with WFP filters: only
// TCP to 127.0.0.1 / ::1 on the proxy port is permitted at the
// ALE_AUTH_CONNECT layer for this package SID, and an explicit block
// filter covers every other connect. The proxy-port permit is
// repeated at ALE_AUTH_RECV_ACCEPT with IsLoopback conditions on both
// endpoints: the built-in AppContainerLoopback block lives in that
// layer and matches both the AppContainer client and the host
// listener, so a connect-layer permit alone would authorize the
// connect and then let the loopback block silently drop the proxy
// connection on either side.
// Because the AppIsolation default does not cover UDP/ICMP, the bind
// layer (ALE_RESOURCE_ASSIGNMENT) permits only TCP binds and blocks
// every other socket type, so no UDP or raw datagram can leave the
// container at all. All filters live in the maximum-weight sublayer,
// evaluated before AppIsolation and Defender, and every permit is hard
// (CLEAR_ACTION_RIGHT) so the built-in blocks cannot override it. The
// proxy applies the allow-list / upstream decisions.
func (n *netIsolation) setupProxy() error {
	proxy, err := corenet.Start(corenet.ProxyConfig{
		Mode:        n.policy.Mode,
		AllowHosts:  n.policy.AllowHosts,
		Rules:       n.policy.Rules,
		Upstream:    n.policy.Proxy,
		TCPLoopback: true,
		MITM:        n.policy.MITM,
		MITMFactory: mitm.New,
	})
	if err != nil {
		return errdefs.Internal(fmt.Errorf("windows: start enforcement proxy: %w", err))
	}
	addr, ok := proxy.Addr().(*net.TCPAddr)
	if !ok {
		_ = proxy.Close()
		return errdefs.Internal(fmt.Errorf(
			"windows: enforcement proxy bound %T, want TCP loopback", proxy.Addr()))
	}
	port := addr.Port

	w, err := newWFPIsolation()
	if err != nil {
		_ = proxy.Close()
		return err
	}
	permitV4 := []wfpCondition{
		sidCondition(n.sid),
		v4AddrCondition(0x7f000001), // 127.0.0.1
		portCondition(uint16(port)),
		tcpProtocolCondition(),
	}
	permitV6 := []wfpCondition{
		sidCondition(n.sid),
		v6LoopbackCondition(), // ::1
		portCondition(uint16(port)),
		tcpProtocolCondition(),
	}
	block := []wfpCondition{sidCondition(n.sid)}
	for _, layer := range []xwin.GUID{fwpmLayerAleAuthConnectV4, fwpmLayerAleAuthConnectV6} {
		conds := permitV4
		if layer == fwpmLayerAleAuthConnectV6 {
			conds = permitV6
		}
		// The proxy-port permit must survive the AppIsolation default
		// deny: a soft permit loses to the built-in block regardless of
		// sublayer order, so this one is made hard (CLEAR_ACTION_RIGHT).
		if err := w.wfpAddFilter(layer, "flowcraft permit enforcement proxy",
			wfpActionPermit, 0xffffffff, wfpFilterFlagClearActionRight, conds); err != nil {
			w.Close()
			_ = proxy.Close()
			return err
		}
		if err := w.wfpAddFilter(layer, "flowcraft block sandbox egress",
			wfpActionBlock, 0xfffffffe, 0, block); err != nil {
			w.Close()
			_ = proxy.Close()
			return err
		}
	}

	// Loopback into the proxy must also be permitted at the
	// AUTH_RECV_ACCEPT layers, where the built-in AppContainerLoopback
	// filter blocks AppContainer loopback (and, per Project Zero's
	// analysis, makes blocked loopback connects time out rather than
	// fail fast). The block's user-ID condition matches both the
	// AppContainer client and the host listener, so two permits are
	// needed: one scoped to the sandbox SID on the client side (remote
	// port == proxy), and one on the host listener side (local port ==
	// proxy, no SID condition because the listener runs as the host
	// user). Both are still pinned to the loopback proxy TCP port, so
	// the sandbox cannot reach arbitrary host loopback services.
	for _, layer := range []xwin.GUID{fwpmLayerAleAuthRecvAcceptV4, fwpmLayerAleAuthRecvAcceptV6} {
		if err := w.wfpAddFilter(layer, "flowcraft permit loopback proxy (client)",
			wfpActionPermit, 0xffffffff, wfpFilterFlagClearActionRight,
			[]wfpCondition{
				sidCondition(n.sid),
				loopbackCondition(),
				portCondition(uint16(port)),
				tcpProtocolCondition(),
			}); err != nil {
			w.Close()
			_ = proxy.Close()
			return err
		}
		if err := w.wfpAddFilter(layer, "flowcraft permit loopback proxy (host)",
			wfpActionPermit, 0xffffffff, wfpFilterFlagClearActionRight,
			[]wfpCondition{
				loopbackCondition(),
				localPortCondition(uint16(port)),
				tcpProtocolCondition(),
			}); err != nil {
			w.Close()
			_ = proxy.Close()
			return err
		}
	}

	bindPermit := []wfpCondition{sidCondition(n.sid), tcpProtocolCondition()}
	bindBlock := []wfpCondition{sidCondition(n.sid)}
	for _, layer := range []xwin.GUID{
		fwpmLayerAleResourceAssignmentV4, fwpmLayerAleResourceAssignmentV6,
	} {
		if err := w.wfpAddFilter(layer, "flowcraft permit tcp bind",
			wfpActionPermit, 0xffffffff, 0, bindPermit); err != nil {
			w.Close()
			_ = proxy.Close()
			return err
		}
		if err := w.wfpAddFilter(layer, "flowcraft block non-tcp bind",
			wfpActionBlock, 0xfffffffe, 0, bindBlock); err != nil {
			w.Close()
			_ = proxy.Close()
			return err
		}
	}

	n.wfp = w
	n.proxy = proxy
	n.proxyPort = port

	if n.policy.MITM != nil && n.policy.MITM.Enabled {
		path, cleanup, err := mitm.WriteBundle(proxy.CAPEM())
		if err != nil {
			return errdefs.Internal(fmt.Errorf("windows: write mitm bundle: %w", err))
		}
		defer cleanup()
		dst := filepath.Join(n.home, "flowcraft-ca.pem")
		if err := copyFile(path, dst); err != nil {
			return errdefs.Internal(fmt.Errorf("windows: stage mitm bundle: %w", err))
		}
		n.bundle = dst
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// env redirects the child's environment away from the user profile,
// which an AppContainer cannot read, into the sandbox home. Existing
// keys are replaced case-insensitively (Windows environment is
// case-insensitive). allow_list / proxy modes also inject the proxy
// environment the way the seatbelt backend does.
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
	if n.proxyPort > 0 {
		httpProxy := fmt.Sprintf("http://127.0.0.1:%d", n.proxyPort)
		socksProxy := fmt.Sprintf("socks5://127.0.0.1:%d", n.proxyPort)
		for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
			repl[k] = httpProxy
		}
		// ALL_PROXY speaks SOCKS5 on the same loopback port; the WFP
		// permit covers TCP to that port regardless of protocol, so
		// SOCKS5-aware non-HTTP clients stay inside the fence too.
		for _, k := range []string{"ALL_PROXY", "all_proxy"} {
			repl[k] = socksProxy
		}
		repl["NO_PROXY"] = ""
		repl["no_proxy"] = ""
	}
	if n.bundle != "" {
		repl["SSL_CERT_FILE"] = n.bundle
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

// Close removes the WFP filters, the enforcement proxy, the
// AppContainer profile, and the sandbox home. It is safe to call more
// than once.
func (n *netIsolation) Close() error {
	var errs []error
	if n.wfp != nil {
		n.wfp.Close()
		n.wfp = nil
	}
	if n.proxy != nil {
		if err := n.proxy.Close(); err != nil {
			errs = append(errs, err)
		}
		n.proxy = nil
	}
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
