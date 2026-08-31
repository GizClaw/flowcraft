//go:build windows

package windows

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

func mustNewRunner(t *testing.T) *Runner {
	t.Helper()
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestExecEcho(t *testing.T) {
	r := mustNewRunner(t)
	res, err := r.Exec(context.Background(), "cmd", []string{"/c", "echo", "hello"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "hello") {
		t.Fatalf("Stdout = %q, want hello", res.Stdout)
	}
}

func TestExecTimeout(t *testing.T) {
	r := mustNewRunner(t)
	_, err := r.Exec(context.Background(), "cmd",
		[]string{"/c", "ping", "-n", "10", "127.0.0.1", ">nul"},
		sandbox.ExecOptions{Timeout: 500 * time.Millisecond})
	if err == nil {
		t.Fatal("Exec succeeded, want timeout")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
}

func TestExecTruncatesOutput(t *testing.T) {
	r := mustNewRunner(t)
	const cap = 1024
	res, err := r.Exec(context.Background(), "cmd",
		[]string{"/c", "for", "/L", "%i", "in", "(1,1,200)", "do", "@echo", "xxxxxxxxxxxxxxxxxxxxxxxx"},
		sandbox.ExecOptions{Resources: sandbox.ResourceLimits{MaxOutputBytes: cap}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if len(res.Stdout) != cap {
		t.Fatalf("truncated Stdout len = %d, want %d", len(res.Stdout), cap)
	}
}

func TestSessionReadAndExit(t *testing.T) {
	r := mustNewRunner(t)
	sess, err := r.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"cmd", "/c", "echo", "hello"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	out, err := readAll(context.Background(), sess)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("output = %q, want hello", out)
	}
	exit, err := sess.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Code != 0 || exit.Reason != sandbox.SessionExited {
		t.Fatalf("exit = %+v, want exited code 0", exit)
	}
}

func TestSessionTerminate(t *testing.T) {
	r := mustNewRunner(t)
	sess, err := r.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"cmd", "/c", "ping", "-n", "20", "127.0.0.1", ">nul"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sess.Terminate(ctx); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	exit, err := sess.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Reason != sandbox.SessionTerminated {
		t.Fatalf("exit = %+v, want SessionTerminated", exit)
	}
}

func TestSessionStdin(t *testing.T) {
	r := mustNewRunner(t)
	sess, err := r.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"cmd", "/c", "more"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if err := sess.Write(context.Background(), []byte("hello world\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sess.CloseInput(); err != nil {
		t.Fatalf("CloseInput: %v", err)
	}
	out, err := readAll(context.Background(), sess)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(out, "hello world") {
		t.Fatalf("output = %q, want echoed stdin", out)
	}
}

func TestRunnerAdvertisesTTY(t *testing.T) {
	r := mustNewRunner(t)
	if !r.Capabilities().Features.TTY {
		t.Fatal("Capabilities().Features.TTY = false, want true")
	}
}

func TestContainedInRootFoldsCase(t *testing.T) {
	cases := []struct {
		path string
		root string
		want bool
	}{
		{`C:\Work\WS`, `c:\work\ws`, true},
		{`C:\Work\WS\sub`, `c:\work\ws`, true},
		{`C:\Work\WS\Sub\file.txt`, `c:\work\ws`, true},
		{`C:\Work\WS2`, `c:\work\ws`, false},    // sibling prefix
		{`C:\Work\WS\x`, `c:\work\ws\y`, false}, // different subtree
	}
	for _, tc := range cases {
		if got := containedInRoot(tc.path, tc.root); got != tc.want {
			t.Errorf("containedInRoot(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
		}
	}
}

func TestResolveWorkDirCaseVariant(t *testing.T) {
	r := mustNewRunner(t)
	// An absolute workdir spelled with a different case than the root
	// must still resolve: EvalExistingPrefix canonicalizes the existing
	// part, and the containment check folds case as a second line of
	// defense.
	alt := strings.ToUpper(r.rootDir)
	got, err := r.resolveWorkDir(alt)
	if err != nil {
		t.Fatalf("resolveWorkDir(case variant) = _, %v", err)
	}
	if !strings.EqualFold(got, r.rootDir) {
		t.Fatalf("resolveWorkDir = %q, want under root %q", got, r.rootDir)
	}
}

func TestPipeSessionReportsNoTTY(t *testing.T) {
	r := mustNewRunner(t)
	sess, err := r.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"cmd", "/c", "ver"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()
	if sess.Capabilities().TTY {
		t.Fatal("pipe session reports TTY = true, want false")
	}
}

func TestSessionTTYEcho(t *testing.T) {
	r := mustNewRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := r.Start(ctx, sandbox.SessionSpec{
		Argv: []string{"cmd", "/c", "echo", "hello", "tty"},
		TTY:  true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if !sess.Capabilities().TTY {
		t.Fatal("Capabilities().TTY = false, want true")
	}
	out, err := readAll(ctx, sess)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(out, "hello tty") {
		t.Fatalf("output = %q, want 'hello tty'", out)
	}
	exit, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Code != 0 || exit.Reason != sandbox.SessionExited {
		t.Fatalf("exit = %+v, want exited code 0", exit)
	}
}

func TestSessionTTYInteractive(t *testing.T) {
	r := mustNewRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := r.Start(ctx, sandbox.SessionSpec{
		Argv: []string{"cmd"},
		TTY:  true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if err := sess.Write(ctx, []byte("echo conpty-hi\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sess.Write(ctx, []byte("exit\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := readAll(ctx, sess)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(out, "conpty-hi") {
		t.Fatalf("output = %q, want echoed 'conpty-hi'", out)
	}
	exit, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Code != 0 || exit.Reason != sandbox.SessionExited {
		t.Fatalf("exit = %+v, want exited code 0", exit)
	}
}

func TestSessionTTYResize(t *testing.T) {
	r := mustNewRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := r.Start(ctx, sandbox.SessionSpec{
		Argv: []string{"cmd"},
		TTY:  true,
		Rows: 30,
		Cols: 100,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if err := sess.Resize(ctx, 40, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := sess.Resize(ctx, 0, 120); err == nil {
		t.Fatal("Resize with zero rows succeeded, want validation error")
	}
	if err := sess.Write(ctx, []byte("exit\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := readAll(ctx, sess); err != nil {
		t.Fatalf("Read: %v", err)
	}
	exit, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Code != 0 {
		t.Fatalf("exit = %+v, want code 0", exit)
	}
}

func TestSessionTTYCloseInputNotAvailable(t *testing.T) {
	r := mustNewRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := r.Start(ctx, sandbox.SessionSpec{
		Argv: []string{"cmd"},
		TTY:  true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if err := sess.CloseInput(); !errdefs.IsNotAvailable(err) {
		t.Fatalf("CloseInput err = %v, want NotAvailable", err)
	}
	if err := sess.Write(ctx, []byte("exit\r\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := readAll(ctx, sess); err != nil {
		t.Fatalf("Read: %v", err)
	}
}

func TestSessionTTYWithConfinementNotAvailable(t *testing.T) {
	r, err := New(t.TempDir(), WithWriteConfinement())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	_, err = r.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"cmd"},
		TTY:  true,
	})
	if err == nil {
		t.Fatal("Start succeeded, want NotAvailable")
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable", err)
	}
}

func TestExecBudgetExceededMemory(t *testing.T) {
	r := mustNewRunner(t)
	_, err := r.Exec(context.Background(), "powershell",
		[]string{"-NoProfile", "-Command", "$a = New-Object byte[] 400MB"},
		sandbox.ExecOptions{
			Timeout: 20 * time.Second,
			Resources: sandbox.ResourceLimits{
				MemoryBytes: 32 << 20,
			},
		})
	if err == nil {
		t.Fatal("Exec succeeded, want budget exceeded")
	}
	if !errdefs.IsBudgetExceeded(err) {
		t.Fatalf("err = %v, want BudgetExceeded", err)
	}
}

func TestExecBudgetExceededCPU(t *testing.T) {
	r := mustNewRunner(t)
	// A tight PowerShell loop burns a full core as user time. The
	// budget is Timeout x millicores / 1000 = 3s of user time, which
	// the loop exhausts in a few seconds of wall clock, well inside
	// the 30s deadline — unlike a cmd `for /L ... do rem` loop, which
	// can idle in kernel waits and miss a tight 10s window.
	_, err := r.Exec(context.Background(), "powershell",
		[]string{"-NoProfile", "-Command", "$x = 0L; while ($true) { $x++ }"},
		sandbox.ExecOptions{
			Timeout: 30 * time.Second,
			Resources: sandbox.ResourceLimits{
				CPUMillicores: 100,
			},
		})
	if err == nil {
		t.Fatal("Exec succeeded, want budget exceeded")
	}
	if !errdefs.IsBudgetExceeded(err) {
		t.Fatalf("err = %v, want BudgetExceeded", err)
	}
}

// readAll drains the session until EOF or ctx is done.
func readAll(ctx context.Context, sess sandbox.Session) (string, error) {
	var b strings.Builder
	after := int64(0)
	for {
		out, err := sess.Read(ctx, after, 64*1024)
		if err != nil {
			return "", err
		}
		for _, ch := range out.Chunks {
			b.Write(ch.Data)
		}
		after = out.NextSeq
		if out.EOF {
			return b.String(), nil
		}
	}
}
