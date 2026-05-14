//go:build linux

package nsjail

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// fakeNsjail writes a tiny shell script that mimics nsjail's argv
// contract: everything up to "--" is its own flags, everything after
// is "cmd args...". The script prints the parsed argv as JSON-like
// lines so tests can assert the translation without depending on a
// real nsjail binary.
func fakeNsjail(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "nsjail")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake nsjail: %v", err)
	}
	return path
}

// echoNsjail returns a fake nsjail that re-emits its own argv on
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
	_, err := New(t.TempDir(), WithBinary("/no/such/nsjail/binary"))
	if err == nil {
		t.Fatalf("expected error for missing binary")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Errorf("expected NotAvailable, got %v", err)
	}
}

func TestRunner_Exec_FlagsAndCmdPassThrough(t *testing.T) {
	bin := fakeNsjail(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Exec(context.Background(), "/bin/echo", []string{"hello"}, sandbox.ExecOptions{
		Timeout: 2 * time.Second,
		Env:     sandbox.EnvPolicy{Allow: []string{}}, // drop host env
		Net:     sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
		Resources: sandbox.ResourceLimits{
			CPUMillicores: 250,
			MemoryBytes:   128 << 20,
		},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d (stderr=%q)", res.ExitCode, res.Stderr)
	}
	stdout := res.Stdout
	for _, want := range []string{
		"ARG:-Mo",
		"ARG:--quiet",
		"ARG:--disable_clone_newns",
		"ARG:--time_limit",
		"ARG:--cgroup_cpu_ms_per_sec",
		"ARG:250",
		"ARG:--cgroup_mem_max",
		"ARG:" + itoa(128<<20),
	} {
		if !strings.Contains(stdout, want+"\n") && !strings.HasSuffix(stdout, want) {
			t.Errorf("missing %q in stdout:\n%s", want, stdout)
		}
	}
	if !strings.Contains(res.Stderr, "CMD:/bin/echo") {
		t.Errorf("expected CMD:/bin/echo in stderr, got %q", res.Stderr)
	}
	// NetDenyAll must NOT add --disable_clone_newnet.
	if strings.Contains(stdout, "ARG:--disable_clone_newnet\n") {
		t.Errorf("NetDenyAll leaked --disable_clone_newnet:\n%s", stdout)
	}
}

func TestRunner_Exec_NetAllowListRejected(t *testing.T) {
	bin := fakeNsjail(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{
		Net: sandbox.NetPolicy{Mode: sandbox.NetAllowList, AllowHosts: []string{"example.com"}},
	})
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Errorf("expected NotAvailable, got %v", err)
	}
}

func TestRunner_Exec_PropagatesNonZeroExit(t *testing.T) {
	failScript := `#!/bin/sh
exit 7
`
	bin := fakeNsjail(t, failScript)
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
	// Fake nsjail that echoes its stdin to its stdout.
	bin := fakeNsjail(t, `#!/bin/sh
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
	bin := fakeNsjail(t, `#!/bin/sh
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
	bin := fakeNsjail(t, echoScript)
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
	bin := fakeNsjail(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin), WithExtraFlags("--bindmount", "/foo:/bar"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Exec(context.Background(), "/bin/true", nil, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "ARG:--bindmount") || !strings.Contains(res.Stdout, "ARG:/foo:/bar") {
		t.Errorf("extra flags not propagated, stdout=%q", res.Stdout)
	}
}

func TestRunner_Exec_ContextCancelled(t *testing.T) {
	bin := fakeNsjail(t, `#!/bin/sh
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
	bin := fakeNsjail(t, echoScript)
	r, err := New(t.TempDir(), WithBinary(bin))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = r.Exec(context.Background(), "", nil, sandbox.ExecOptions{})
	if err == nil {
		t.Fatalf("expected validation error for empty command")
	}
}

func itoa(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
