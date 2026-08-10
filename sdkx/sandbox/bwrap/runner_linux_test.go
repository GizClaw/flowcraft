//go:build linux

package bwrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	"github.com/GizClaw/flowcraft/sdkx/sandbox/bwrap/internal/bridge"
)

// fakeBwrap writes a tiny shell script that mimics bwrap's argv
// contract: everything up to "--" is its own flags, everything after
// is "cmd args...". The script prints the parsed argv as JSON-like
// lines so tests can assert the translation without depending on a
// real bwrap binary.
func fakeBwrap(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bwrap: %v", err)
	}
	return path
}

// echoScript returns a fake bwrap that re-emits its own argv on
// stdout, one per line, prefixed with "ARG:", and the post-"--"
// command's name on stderr. This makes both translation and
// command-pass-through observable in ExecResult.
const echoScript = `#!/bin/sh
seen_sep=0
for a in "$@"; do
  if [ "$seen_sep" = "1" ]; then
    echo "CMD:$a" 1>&2
    seen_sep=2
    continue
  fi
  if [ "$a" = "--" ]; then
    seen_sep=1
    continue
  fi
  echo "ARG:$a"
done
exit 0
`

func TestNew_BinaryNotFound(t *testing.T) {
	_, err := New(t.TempDir(), WithBinary("/no/such/bwrap/binary"))
	if err == nil {
		t.Fatalf("expected error for missing binary")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Errorf("expected NotAvailable, got %v", err)
	}
}

func TestRunner_EnforcementIncludesFilesystemBounds(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatal(err)
	}
	enforcement := r.Enforcement()
	if !enforcement.FilesystemBounds {
		t.Errorf("filesystem mount boundary must be reported: %+v", enforcement)
	}
}

func TestRunner_Exec_FlagsAndCmdPassThrough(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	root := t.TempDir()
	r, err := New(root, WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Exec(context.Background(), "/bin/echo", []string{"hello"}, sandbox.ExecOptions{
		Env: sandbox.EnvPolicy{Allow: []string{}}, // drop host env
		Net: sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d (stderr=%q)", res.ExitCode, res.Stderr)
	}
	stdout := res.Stdout
	for _, want := range []string{
		"ARG:--die-with-parent",
		"ARG:--unshare-pid",
		"ARG:--clearenv",
		"ARG:--unshare-net",
		"ARG:--ro-bind",
		"ARG:--tmpfs",
		"ARG:/tmp",
		"ARG:--bind",
		"ARG:" + root,
		"ARG:--proc",
		"ARG:--dev",
	} {
		if !strings.Contains(stdout, want+"\n") && !strings.HasSuffix(stdout, want) {
			t.Errorf("missing %q in stdout:\n%s", want, stdout)
		}
	}
	if !strings.Contains(res.Stderr, "CMD:/bin/echo") {
		t.Errorf("expected CMD:/bin/echo in stderr, got %q", res.Stderr)
	}
	// NetDenyAll must NOT add --share-net.
	if strings.Contains(stdout, "ARG:--share-net\n") {
		t.Errorf("NetDenyAll leaked --share-net:\n%s", stdout)
	}
	// Env must not leak host vars under Allow=[].
	if strings.Contains(stdout, "ARG:--setenv\n") {
		t.Errorf("Allow=[] leaked --setenv entries:\n%s", stdout)
	}
}

func TestRunner_Exec_NetAllowListWiresBridge(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{
		Net: sandbox.NetPolicy{Mode: sandbox.NetAllowList, AllowHosts: []string{"example.com"}},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	stdout := res.Stdout
	for _, want := range []string{
		"ARG:--unshare-net",
		"ARG:--tmpfs",
		"ARG:/run",
		"ARG:--bind",
		"ARG:/run/flowcraft-proxy.sock",
		"ARG:--ro-bind",
	} {
		if !strings.Contains(stdout, want+"\n") && !strings.HasSuffix(stdout, want) {
			t.Errorf("missing %q in stdout:\n%s", want, stdout)
		}
	}
	// The bridge is the running executable re-executed with the
	// reserved marker, not a separate binary.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if !strings.Contains(res.Stderr, "CMD:"+exe+"\n") {
		t.Errorf("expected the host executable as the bridge command, stderr=%q", res.Stderr)
	}
	if !strings.Contains(stdout, "ARG:"+bridge.Marker+"\n") {
		t.Errorf("expected bridge marker after the executable, stdout=%q", stdout)
	}
	if !strings.Contains(res.Stderr, "CMD:/bin/true\n") {
		t.Errorf("expected the real command to pass through the bridge, stderr=%q", res.Stderr)
	}
}

func TestRunner_Exec_NetProxyWiresBridge(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{
		Net: sandbox.NetPolicy{Mode: sandbox.NetProxy, Proxy: "http://127.0.0.1:9999"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if !strings.Contains(res.Stderr, "CMD:"+exe+"\n") {
		t.Errorf("expected the host executable as the bridge command, stderr=%q", res.Stderr)
	}
	if !strings.Contains(res.Stdout, "ARG:"+bridge.Marker+"\n") {
		t.Errorf("expected bridge marker after the executable, stdout=%q", res.Stdout)
	}
}

func TestRunner_EnforcementListsNetworkModes(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	enforcement := r.Enforcement()
	for _, want := range []sandbox.NetMode{sandbox.NetDenyAll, sandbox.NetAllowList, sandbox.NetProxy} {
		found := false
		for _, mode := range enforcement.NetModes {
			if mode == want {
				found = true
			}
		}
		if !found {
			t.Errorf("Enforcement().NetModes missing %d: %v", int(want), enforcement.NetModes)
		}
	}
}

func TestRunner_Exec_DiskBytesRejected(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{
		Resources: sandbox.ResourceLimits{DiskBytes: 1 << 20},
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Errorf("expected NotAvailable, got %v", err)
	}
}

func TestRunner_Exec_CPUMillicoresRequiresTimeout(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{
		Resources: sandbox.ResourceLimits{CPUMillicores: 500},
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Errorf("expected NotAvailable, got %v", err)
	}
}

func TestRunner_Exec_PropagatesNonZeroExit(t *testing.T) {
	failScript := `#!/bin/sh
exit 7
`
	bin := fakeBwrap(t, failScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec returned err for non-zero exit (should be result-not-error): %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("expected ExitCode 7, got %d", res.ExitCode)
	}
}

func TestRunner_Exec_HonoursStdin(t *testing.T) {
	// Fake bwrap that echoes its stdin to its stdout.
	bin := fakeBwrap(t, `#!/bin/sh
cat
exit 0
`)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{
		Stdin: []byte("hello-stdin"),
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "hello-stdin" {
		t.Errorf("stdin not forwarded: stdout=%q", res.Stdout)
	}
}

func TestRunner_Exec_TruncatesOutput(t *testing.T) {
	bin := fakeBwrap(t, `#!/bin/sh
# Print 4096 'a' bytes then exit.
printf '%4096s' "" | tr ' ' 'a'
exit 0
`)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{
		Resources: sandbox.ResourceLimits{MaxOutputBytes: 100},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := len(res.Stdout); got != 100 {
		t.Errorf("expected truncated to 100 bytes, got %d", got)
	}
}

func TestRunner_Exec_WorkDirEscapeRejected(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	root := t.TempDir()
	r, err := New(root, WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{
		WorkDir: "/etc",
	})
	if err == nil {
		t.Fatalf("expected escape rejection")
	}
}

func TestRunner_Exec_ExtraFlagsPropagated(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin), WithExtraFlags("--level-prefix"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "ARG:--level-prefix\n") {
		t.Errorf("extra flags not propagated, stdout=%q", res.Stdout)
	}
}

func TestNew_RejectsWeakeningExtraFlags(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	for _, flag := range []string{
		"--ro-bind", "--bind", "--tmpfs", "--proc", "--dev",
		"--clearenv", "--setenv", "--unsetenv",
		"--share-net", "--unshare-net", "--unshare-all", "--unshare-pid",
		"--chdir", "--seccomp", "--cap-drop", "--new-session", "--args",
		"--ro-bind=/x:/y", "--setenv=PATH=/x",
	} {
		t.Run(strings.TrimLeft(flag, "-"), func(t *testing.T) {
			_, err := New(t.TempDir(), WithBinary(bin), WithExtraFlags(flag))
			if !errdefs.IsValidation(err) {
				t.Fatalf("flag %q: expected Validation, got %v", flag, err)
			}
		})
	}
}

func TestRunner_WithWritablePaths(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	cache := t.TempDir()
	r, err := New(t.TempDir(), WithBinary(bin), WithWritablePaths(cache))
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "ARG:--bind\nARG:"+cache+"\n") {
		t.Errorf("writable path not emitted as bind mount: %q", res.Stdout)
	}
}

func TestRunner_Exec_ContextCancelled(t *testing.T) {
	bin := fakeBwrap(t, `#!/bin/sh
sleep 5
exit 0
`)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = r.Exec(ctx, "/bin/true", nil, sandbox.ExecOptions{})
	if err == nil {
		t.Fatalf("expected cancellation error")
	}
	if !errdefs.IsTimeout(err) && !errdefs.IsAborted(err) {
		t.Errorf("expected timeout/aborted error, got %v", err)
	}
}

func TestRunner_Exec_EmptyCommandRejected(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Exec(context.Background(), "", nil, sandbox.ExecOptions{})
	if err == nil {
		t.Fatalf("expected validation error for empty command")
	}
}

func TestRunner_ProcessManager_PipeSession(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proc, err := r.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/echo", "hi"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var sb strings.Builder
	var seq int64
	for {
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
	if !strings.Contains(sb.String(), "CMD:/bin/echo") || !strings.Contains(sb.String(), "CMD:hi") {
		t.Fatalf(`session output missing post-"--" argv: %q`, sb.String())
	}
	if exit, err := proc.Wait(ctx); err != nil || exit.Code != 0 {
		t.Fatalf("Wait = %+v, %v; want exited(0)", exit, err)
	}
}

func TestRunner_ProcessManager_TTYSession(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proc, err := r.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/echo", "hi"},
		TTY:  true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var sb strings.Builder
	var seq int64
	for {
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
	if !strings.Contains(sb.String(), "CMD:/bin/echo") {
		t.Fatalf("TTY session output missing argv: %q", sb.String())
	}
	if exit, err := proc.Wait(ctx); err != nil || exit.Code != 0 {
		t.Fatalf("Wait = %+v, %v; want exited(0)", exit, err)
	}
}

func TestRunner_ProcessManager_PolicyRejected(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/true"},
		Opts: sandbox.ExecOptions{Resources: sandbox.ResourceLimits{DiskBytes: 1}},
	})
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("disk-limit Start = %v, want NotAvailable", err)
	}
}

func TestRunner_Enforcement_ProxyFeatures(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e := r.Enforcement()
	if !e.Socks5 || !e.MITM || !e.UnixSocketPolicy {
		t.Errorf("bwrap must claim socks5/mitm/unix-socket policy, got %+v", e)
	}
}

func TestRunner_ProcessManager_UnixSocketBindFlags(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	sock := filepath.Join(t.TempDir(), "test.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatalf("create socket file: %v", err)
	}
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proc, err := r.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/echo", "hi"},
		Opts: sandbox.ExecOptions{Net: sandbox.NetPolicy{
			UnixSockets: []string{sock},
		}},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var sb strings.Builder
	var seq int64
	for {
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
	if !strings.Contains(sb.String(), "ARG:--bind\nARG:"+sock+"\n") {
		t.Fatalf("unix socket bind not emitted: %q", sb.String())
	}
}

func TestRunner_ProcessManager_UnixSocketMissing_NotFound(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/true"},
		Opts: sandbox.ExecOptions{Net: sandbox.NetPolicy{
			UnixSockets: []string{"/no/such/socket"},
		}},
	})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("missing socket Start = %v, want NotFound", err)
	}
}

func TestRunner_Exec_UnixSocketMissing_NotFound(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{
		Net: sandbox.NetPolicy{UnixSockets: []string{"/no/such/socket"}},
	})
	if !errdefs.IsNotFound(err) {
		t.Fatalf("missing socket Exec = %v, want NotFound", err)
	}
}

func TestRunner_ProcessManager_MITMBundleInjected(t *testing.T) {
	bin := fakeBwrap(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proc, err := r.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/echo", "hi"},
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var sb strings.Builder
	var seq int64
	for {
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
	if !strings.Contains(sb.String(), "ARG:SSL_CERT_FILE") || !strings.Contains(sb.String(), "ARG:--ro-bind") {
		t.Fatalf("MITM CA bundle env/bind missing: %q", sb.String())
	}
}
