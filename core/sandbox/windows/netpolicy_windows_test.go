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
	iso, err := newNetIsolation(t.TempDir(), nil, corenet.NetDenyAll)
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

func TestNetPolicyUnsupportedModeNotAvailable(t *testing.T) {
	r := mustNewRunner(t)
	_, err := r.Exec(context.Background(), "cmd", []string{"/c", "ver"},
		sandbox.ExecOptions{Net: corenet.NetPolicy{Mode: corenet.NetAllowList}})
	if err == nil {
		t.Fatal("Exec succeeded, want NotAvailable")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
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
