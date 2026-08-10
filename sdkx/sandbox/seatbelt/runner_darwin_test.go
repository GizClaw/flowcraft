//go:build darwin

package seatbelt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func TestRunner_Enforcement(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := runner.Enforcement()
	if !got.EnvAllowList || !got.FilesystemBounds {
		t.Errorf("Seatbelt profile covers env and filesystem: %+v", got)
	}
	// Resource caps do not come from the Seatbelt profile — they come
	// from the shared process-group sampler — so the claim must follow
	// that sampler's real operability rather than being hardcoded.
	if want := sandbox.GroupCapsSupported(); got.MemoryCap != want || got.CPUCap != want {
		t.Errorf("MemoryCap=%v CPUCap=%v, want both %v (GroupCapsSupported)",
			got.MemoryCap, got.CPUCap, want)
	}
	if got.DiskCap {
		t.Error("Seatbelt must not claim disk quotas")
	}
	wantModes := []sandbox.NetMode{sandbox.NetDenyAll, sandbox.NetAllowList, sandbox.NetProxy}
	if len(got.NetModes) != len(wantModes) {
		t.Fatalf("NetModes = %v, want %v", got.NetModes, wantModes)
	}
	for i, want := range wantModes {
		if got.NetModes[i] != want {
			t.Errorf("NetModes[%d] = %v, want %v", i, got.NetModes[i], want)
		}
	}
}

func TestRunner_ExecAndEnvironment(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := runner.Exec(
		context.Background(),
		"/bin/sh",
		[]string{"-c", `printf "%s:%s" "$FLOWCRAFT_TEST" "${HOME-unset}"`},
		sandbox.ExecOptions{
			Env: sandbox.EnvPolicy{
				Allow:  []string{},
				Inject: map[string]string{"FLOWCRAFT_TEST": "ok"},
			},
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != "ok:unset" {
		t.Fatalf("result = %+v, want stdout ok:unset", result)
	}
}

func TestRunner_WorkDirEscapeRejected(t *testing.T) {
	root := t.TempDir()
	runner, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Exec(context.Background(), "/bin/pwd", nil, sandbox.ExecOptions{
		WorkDir: t.TempDir(),
	})
	if !errors.Is(err, sandbox.ErrPathTraversal) {
		t.Fatalf("expected ErrPathTraversal, got %v", err)
	}
}

func TestRunner_UnsupportedPolicies(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]sandbox.ExecOptions{
		"disk": {Resources: sandbox.ResourceLimits{DiskBytes: 1}},
		"cpu-without-timeout": {
			Resources: sandbox.ResourceLimits{CPUMillicores: 100},
		},
		"unknown-net-mode": {
			Net: sandbox.NetPolicy{Mode: sandbox.NetMode(99)},
		},
		"unix sockets": {
			Net: sandbox.NetPolicy{UnixSockets: []string{"/tmp/test.sock"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runner.Exec(context.Background(), "/usr/bin/true", nil, opts)
			if !errdefs.IsNotAvailable(err) {
				t.Fatalf("expected NotAvailable, got %v", err)
			}
		})
	}
}

func TestRunner_FilesystemWriteBound(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	runner, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "inside.txt")
	out := filepath.Join(outside, "outside.txt")
	script := `printf inside > "$1"; printf outside > "$2"`
	result, err := runner.Exec(context.Background(), "/bin/sh",
		[]string{"-c", script, "sh", inside, out},
		sandbox.ExecOptions{Timeout: 5 * time.Second},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("outside write should make the shell fail: %+v", result)
	}
	if data, err := os.ReadFile(inside); err != nil || string(data) != "inside" {
		t.Fatalf("inside write failed: data=%q err=%v", data, err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("outside file should not exist, stat err=%v", err)
	}
	if !strings.Contains(result.Stderr, "Operation not permitted") {
		t.Logf("Seatbelt rejection stderr differed: %q", result.Stderr)
	}
}

func TestRunner_ProcessManager_PipeSession(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/sh", "-c", "printf OUT; printf ERR >&2; exit 7"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var stdout, stderr strings.Builder
	var seq int64
	for {
		out, err := proc.Read(ctx, seq, 4096)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for _, ch := range out.Chunks {
			switch ch.Stream {
			case sandbox.ProcessStreamStdout:
				stdout.Write(ch.Data)
			case sandbox.ProcessStreamStderr:
				stderr.Write(ch.Data)
			}
		}
		seq = out.NextSeq
		if out.EOF {
			break
		}
	}
	if stdout.String() != "OUT" || stderr.String() != "ERR" {
		t.Fatalf("stdout=%q stderr=%q, want OUT/ERR", stdout.String(), stderr.String())
	}
	exit, err := proc.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Code != 7 || exit.Reason != sandbox.ProcessExited {
		t.Fatalf("exit = %+v, want exited(7)", exit)
	}
}

func TestRunner_ProcessManager_TTYSession(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/sh"},
		TTY:  true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := proc.Write(ctx, []byte("printf ok\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	var sb strings.Builder
	var seq int64
	for !strings.Contains(sb.String(), "ok") {
		out, err := proc.Read(ctx, seq, 4096)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for _, ch := range out.Chunks {
			sb.Write(ch.Data)
		}
		seq = out.NextSeq
		if out.EOF {
			break
		}
	}
	if !strings.Contains(sb.String(), "ok") {
		t.Fatalf("TTY output missing 'ok': %q", sb.String())
	}
	if err := proc.Write(ctx, []byte("exit\n")); err != nil {
		t.Fatalf("Write exit: %v", err)
	}
	if exit, err := proc.Wait(ctx); err != nil || exit.Code != 0 {
		t.Fatalf("Wait = %+v, %v; want exited(0)", exit, err)
	}
}

func TestRunner_ProcessManager_PolicyRejected(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/usr/bin/true"},
		Opts: sandbox.ExecOptions{Resources: sandbox.ResourceLimits{DiskBytes: 1}},
	})
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("disk-limit Start = %v, want NotAvailable", err)
	}
}

func TestRunner_Enforcement_ProxyFeatures(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e := runner.Enforcement()
	if !e.Socks5 || !e.MITM {
		t.Errorf("seatbelt proxy supports socks5 + MITM host-side, got Socks5=%v MITM=%v", e.Socks5, e.MITM)
	}
	if e.UnixSocketPolicy {
		t.Error("seatbelt must not claim unix socket policy (SBPL does not confine unix sockets)")
	}
}

func TestRunner_ProcessManager_UnixSocketsNotAvailable(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/usr/bin/true"},
		Opts: sandbox.ExecOptions{Net: sandbox.NetPolicy{
			UnixSockets: []string{"/tmp/test.sock"},
		}},
	})
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("unix socket policy Start = %v, want NotAvailable", err)
	}
}

func TestRunner_ProcessManager_MITMStart(t *testing.T) {
	runner, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/usr/bin/true"},
		Opts: sandbox.ExecOptions{Net: sandbox.NetPolicy{
			Mode:       sandbox.NetAllowList,
			AllowHosts: []string{"example.com"},
			MITM:       &sandbox.MITMPolicy{Enabled: true},
		}},
	})
	if err != nil {
		t.Fatalf("Start with MITM: %v", err)
	}
	defer func() { _ = proc.Close() }()
	exit, err := proc.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Code != 0 {
		t.Fatalf("exit = %+v, want 0", exit)
	}
}

func TestBuildEnv_ProxyModeInjectsAndStrips(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://host-proxy:3128")
	t.Setenv("NO_PROXY", "localhost")
	t.Setenv("KEEP", "kept")
	env := buildEnv(sandbox.EnvPolicy{Allow: nil}, 43123)

	got := map[string]string{}
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if ok {
			got[key] = value
		}
	}
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if got[name] != "http://127.0.0.1:43123" {
			t.Errorf("%s = %q, want loopback proxy", name, got[name])
		}
	}
	if got["NO_PROXY"] != "" || got["no_proxy"] != "" {
		t.Errorf("NO_PROXY not stripped: %q %q", got["NO_PROXY"], got["no_proxy"])
	}
	if got["KEEP"] != "kept" {
		t.Errorf("non-proxy env dropped: %v", got)
	}
}

func TestBuildEnv_NoProxyModeLeavesEnv(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://host-proxy:3128")
	t.Setenv("NO_PROXY", "localhost")
	env := buildEnv(sandbox.EnvPolicy{Allow: nil}, 0)
	got := map[string]string{}
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if ok {
			got[key] = value
		}
	}
	if got["HTTP_PROXY"] != "http://host-proxy:3128" {
		t.Errorf("HTTP_PROXY changed without proxy mode: %q", got["HTTP_PROXY"])
	}
	if got["NO_PROXY"] != "localhost" {
		t.Errorf("NO_PROXY changed without proxy mode: %q", got["NO_PROXY"])
	}
}
