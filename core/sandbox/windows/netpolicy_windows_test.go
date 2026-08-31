//go:build windows

package windows

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	corenet "github.com/GizClaw/flowcraft/core/utils/net"
)

// requireNetIsolation probes whether the host can create AppContainer
// profiles. Non-elevated hosts fail closed with NotAvailable; the
// net-policy behaviors cannot be exercised there, so the test skips.
func requireNetIsolation(t *testing.T) {
	t.Helper()
	iso, err := newNetIsolation(t.TempDir(), nil, corenet.NetPolicy{Mode: corenet.NetDenyAll})
	if err != nil {
		if errdefs.IsNotAvailable(err) {
			t.Skipf("host cannot create appcontainer profiles: %v", err)
		}
		t.Fatalf("net isolation probe: %v", err)
	}
	if err := iso.Close(); err != nil {
		t.Fatalf("net isolation probe close: %v", err)
	}
}

// requireWFP probes whether the host can open the WFP engine, which
// allow_list / proxy modes need. Non-elevated hosts fail closed with
// NotAvailable.
func requireWFP(t *testing.T) {
	t.Helper()
	w, err := newWFPIsolation()
	if err != nil {
		if errdefs.IsNotAvailable(err) {
			t.Skipf("host cannot open the WFP engine: %v", err)
		}
		t.Fatalf("wfp probe: %v", err)
	}
	w.Close()
}

func TestExecNetDenyAll(t *testing.T) {
	requireNetIsolation(t)
	r := mustNewRunner(t)
	res, err := r.Exec(context.Background(), "cmd",
		[]string{"/c", "echo", "deny-ok"},
		sandbox.ExecOptions{Net: corenet.NetPolicy{Mode: corenet.NetDenyAll}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "deny-ok") {
		t.Fatalf("Stdout = %q, want deny-ok", res.Stdout)
	}
}

// TestExecNetDenyAllBlocksNetwork is the differential check: the same
// outbound TCP dial succeeds without a policy and fails under
// NetDenyAll, proving the AppContainer has no network reach.
func TestExecNetDenyAllBlocksNetwork(t *testing.T) {
	requireNetIsolation(t)
	r := mustNewRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dial := []string{"-NoProfile", "-NonInteractive", "-Command",
		"try { (New-Object Net.Sockets.TcpClient).Connect('1.1.1.1',80); Write-Output ok } catch { Write-Output fail; exit 1 }"}

	ctrl, err := r.Exec(ctx, "powershell", dial, sandbox.ExecOptions{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("control Exec: %v", err)
	}
	if ctrl.ExitCode != 0 || !strings.Contains(ctrl.Stdout, "ok") {
		t.Skipf("host cannot reach the internet without a policy (exit=%d out=%q); skipping", ctrl.ExitCode, ctrl.Stdout)
	}

	denied, err := r.Exec(ctx, "powershell", dial,
		sandbox.ExecOptions{Timeout: 20 * time.Second, Net: corenet.NetPolicy{Mode: corenet.NetDenyAll}})
	if err != nil {
		t.Fatalf("deny-all Exec: %v", err)
	}
	if denied.ExitCode == 0 && strings.Contains(denied.Stdout, "ok") {
		t.Fatalf("NetDenyAll exec reached the network: stdout=%q", denied.Stdout)
	}
}

func TestNetPolicyTTYNotAvailable(t *testing.T) {
	r := mustNewRunner(t)
	_, err := r.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"cmd"},
		TTY:  true,
		Opts: sandbox.ExecOptions{Net: corenet.NetPolicy{Mode: corenet.NetDenyAll}},
	})
	if err == nil {
		t.Fatal("Start succeeded, want NotAvailable")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}
}

func TestExecNetAllowList(t *testing.T) {
	requireNetIsolation(t)
	requireWFP(t)
	r := mustNewRunner(t)
	res, err := r.Exec(context.Background(), "cmd", []string{"/c", "echo", "allow-ok"},
		sandbox.ExecOptions{Net: corenet.NetPolicy{
			Mode:       corenet.NetAllowList,
			AllowHosts: []string{"1.1.1.1"},
		}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "allow-ok") {
		t.Fatalf("exit=%d stdout=%q, want allow-ok", res.ExitCode, res.Stdout)
	}
}

// TestNetAllowListPinsToProxy verifies the WFP layer: with
// allow_list mode the container may only reach the enforcement proxy
// port, so a raw outbound TCP dial (which ignores proxy env) fails
// even though the container carries the InternetClient capability.
func TestNetAllowListPinsToProxy(t *testing.T) {
	requireNetIsolation(t)
	requireWFP(t)
	r := mustNewRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dial := []string{"-NoProfile", "-NonInteractive", "-Command",
		"try { (New-Object Net.Sockets.TcpClient).Connect('1.1.1.1',80); Write-Output ok } catch { Write-Output fail; exit 1 }"}
	res, err := r.Exec(ctx, "powershell", dial,
		sandbox.ExecOptions{Timeout: 20 * time.Second, Net: corenet.NetPolicy{
			Mode:       corenet.NetAllowList,
			AllowHosts: []string{"1.1.1.1"},
		}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode == 0 && strings.Contains(res.Stdout, "ok") {
		t.Fatalf("allow-list exec reached the network directly: stdout=%q", res.Stdout)
	}
}

// TestNetAllowListBlocksUDP is the differential UDP check: ALE
// filters classify the first send from an unconnected UDP socket at
// AUTH_CONNECT, so a raw UDP sendto under allow-list must fail even
// though the container carries the internetClient capability (DNS
// tunneling via unconnected UDP is not a valid egress path).
func TestNetAllowListBlocksUDP(t *testing.T) {
	requireNetIsolation(t)
	requireWFP(t)
	r := mustNewRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	send := []string{"-NoProfile", "-NonInteractive", "-Command",
		"$u = New-Object Net.Sockets.UdpClient; try { $u.Send([byte[]](0),1,'1.1.1.1',53); Write-Output ok } catch { Write-Output fail; exit 1 }"}

	ctrl, err := r.Exec(ctx, "powershell", send, sandbox.ExecOptions{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatalf("control Exec: %v", err)
	}
	if ctrl.ExitCode != 0 || !strings.Contains(ctrl.Stdout, "ok") {
		t.Skipf("host cannot send UDP without a policy (exit=%d out=%q); skipping", ctrl.ExitCode, ctrl.Stdout)
	}

	denied, err := r.Exec(ctx, "powershell", send,
		sandbox.ExecOptions{Timeout: 20 * time.Second, Net: corenet.NetPolicy{
			Mode:       corenet.NetAllowList,
			AllowHosts: []string{"1.1.1.1"},
		}})
	if err != nil {
		t.Fatalf("allow-list Exec: %v", err)
	}
	if denied.ExitCode == 0 && strings.Contains(denied.Stdout, "ok") {
		t.Fatalf("allow-list exec sent UDP externally: stdout=%q", denied.Stdout)
	}
}

func TestExecNetProxy(t *testing.T) {
	requireNetIsolation(t)
	requireWFP(t)
	r := mustNewRunner(t)
	res, err := r.Exec(context.Background(), "cmd", []string{"/c", "echo", "proxy-ok"},
		sandbox.ExecOptions{Net: corenet.NetPolicy{
			Mode:  corenet.NetProxy,
			Proxy: "http://127.0.0.1:1",
		}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "proxy-ok") {
		t.Fatalf("exit=%d stdout=%q, want proxy-ok", res.ExitCode, res.Stdout)
	}
}

func TestNetIsolationEnvRedirect(t *testing.T) {
	iso := &netIsolation{home: `C:\ac-home`}
	env := iso.env([]string{
		"PATH=C:\\bin",
		"TEMP=C:\\Users\\me\\Temp",
		"HOME=C:\\Users\\me",
		"KEEP=1",
	})
	got := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("bad env entry %q", kv)
		}
		got[k] = v
	}
	want := map[string]string{
		"PATH":         "C:\\bin",
		"KEEP":         "1",
		"TEMP":         `C:\ac-home\Temp`,
		"TMP":          `C:\ac-home\Temp`,
		"HOME":         `C:\ac-home`,
		"USERPROFILE":  `C:\ac-home`,
		"APPDATA":      `C:\ac-home\AppData\Roaming`,
		"LOCALAPPDATA": `C:\ac-home\AppData\Local`,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (env %v)", k, got[k], v, env)
		}
	}
}
