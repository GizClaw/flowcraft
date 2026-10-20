package sandbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/local"
)

// drainSession reads the session's output to EOF and returns it. Read
// blocks until data, EOF or ctx, so the order relative to Wait does not
// matter; the log stays readable after the process exits.
func drainSession(t *testing.T, ctx context.Context, sess sandbox.Session) string {
	t.Helper()
	var out []byte
	afterSeq := int64(0)
	for {
		output, err := sess.Read(ctx, afterSeq, 64*1024)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for _, chunk := range output.Chunks {
			out = append(out, chunk.Data...)
		}
		afterSeq = output.NextSeq
		if output.EOF {
			return string(out)
		}
	}
}

func TestRunnerCapabilities(t *testing.T) {
	r := local.New(t.TempDir())
	caps := r.Capabilities()

	if !caps.Policy.EnvAllowList {
		t.Error("EnvAllowList = false, want true")
	}
	if caps.Policy.MemoryCap != sandbox.GroupCapsSupported() {
		t.Errorf("MemoryCap = %v, want GroupCapsSupported() = %v",
			caps.Policy.MemoryCap, sandbox.GroupCapsSupported())
	}
	if caps.Policy.CPUCap != sandbox.GroupCapsSupported() {
		t.Errorf("CPUCap = %v, want GroupCapsSupported() = %v",
			caps.Policy.CPUCap, sandbox.GroupCapsSupported())
	}
	// Sessions either exist on the platform with the full local feature
	// set, or not at all — the three flags must never diverge for
	// local.Runner.
	if caps.Features.TTY != caps.Features.Signal ||
		caps.Features.Signal != caps.Features.Events {
		t.Errorf("session features diverge: %+v", caps.Features)
	}
}

func TestStartReturnsSessionWithCapabilities(t *testing.T) {
	skipOnWindows(t)
	ctx := context.Background()
	r := local.New(t.TempDir())

	sess, err := r.Start(ctx, sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "echo hi"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	caps := sess.Capabilities()
	if !caps.Signal || !caps.Events {
		t.Errorf("pipe session capabilities = %+v, want Signal+Events", caps)
	}
	if caps.TTY {
		t.Errorf("pipe session reports TTY = true")
	}

	out, err := sess.Read(ctx, 0, 64*1024)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(out.Chunks) == 0 || string(out.Chunks[0].Data) != "hi\n" {
		t.Fatalf("output = %+v, want hi\\n", out.Chunks)
	}
	exit, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Code != 0 {
		t.Fatalf("exit = %+v, want code 0", exit)
	}
}

func TestSessionWatch(t *testing.T) {
	skipOnWindows(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := local.New(t.TempDir())

	sess, err := r.Start(ctx, sandbox.SessionSpec{
		Argv: []string{"sh", "-c", "echo out; sleep 0.2; echo done"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	watcher, err := sess.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = watcher.Close() }()

	var sawOutput, sawExited bool
	for {
		select {
		case ev, ok := <-watcher.Events():
			if !ok {
				t.Fatal("watcher channel closed before exit event")
			}
			switch ev.Type {
			case sandbox.SessionEventOutput:
				sawOutput = true
			case sandbox.SessionEventExited:
				sawExited = true
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for watcher events")
		}
		if sawOutput && sawExited {
			break
		}
	}
}

func TestSessionSignal(t *testing.T) {
	skipOnWindows(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := local.New(t.TempDir())

	sess, err := r.Start(ctx, sandbox.SessionSpec{
		Argv: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if err := sess.Signal(ctx, sandbox.SessionSignalInterrupt); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	exit, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait after signal: %v", err)
	}
	if exit.Code == 0 && exit.Reason == sandbox.SessionExited {
		t.Fatalf("exit = %+v, want non-zero/signaled after interrupt", exit)
	}
}

// TestRunnerCloseTerminatesSessions verifies that Runner.Close drains
// every active session: after Close, no session started through the
// runner is still running.
func TestRunnerCloseTerminatesSessions(t *testing.T) {
	skipOnWindows(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := local.New(t.TempDir())

	sess, err := r.Start(ctx, sandbox.SessionSpec{
		Argv: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	exit, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait after Close: %v", err)
	}
	if exit.Code == 0 && exit.Reason == sandbox.SessionExited {
		t.Fatalf("exit = %+v, want non-zero/signaled after runner Close", exit)
	}

	// A second Close must be safe and must not error.
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestSessionWithoutStdinReadsEOF is the session half of the fd 0 fix:
// a child that reads stdin with no declaration must see EOF and exit,
// not block on a pipe nobody will ever write.
func TestSessionWithoutStdinReadsEOF(t *testing.T) {
	skipOnWindows(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := local.New(t.TempDir()).Start(ctx, sandbox.SessionSpec{
		Argv: []string{"cat"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	exit, err := sess.Wait(waitCtx)
	if err != nil {
		t.Fatalf("Wait: %v (the child is blocked reading an empty stdin?)", err)
	}
	if exit.Code != 0 {
		t.Fatalf("exit = %+v, want code 0", exit)
	}
	if out := drainSession(t, ctx, sess); out != "" {
		t.Fatalf("output = %q, want empty", out)
	}
}

// TestSessionWithoutStdinRejectsWrite covers the declared surface: no
// input side means Capabilities.Stdin is false, Write says so with a
// self-explaining NotAvailable, and CloseInput stays a no-op success so
// Exec can call it unconditionally.
func TestSessionWithoutStdinRejectsWrite(t *testing.T) {
	skipOnWindows(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := local.New(t.TempDir()).Start(ctx, sandbox.SessionSpec{
		Argv: []string{"sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if sess.Capabilities().Stdin {
		t.Error("Capabilities().Stdin = true, want false without SessionSpec.Stdin")
	}
	if err := sess.Write(ctx, []byte("x")); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Write error = %v, want NotAvailable", err)
	}
	if err := sess.CloseInput(); err != nil {
		t.Fatalf("CloseInput = %v, want nil", err)
	}
}

// TestSessionWithStdinStreams is the positive control: a declared input
// side still streams bytes into the child.
func TestSessionWithStdinStreams(t *testing.T) {
	skipOnWindows(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := local.New(t.TempDir()).Start(ctx, sandbox.SessionSpec{
		Argv:  []string{"cat"},
		Stdin: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if !sess.Capabilities().Stdin {
		t.Error("Capabilities().Stdin = false, want true for a declared input side")
	}
	if err := sess.Write(ctx, []byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sess.CloseInput(); err != nil {
		t.Fatalf("CloseInput: %v", err)
	}
	if out := drainSession(t, ctx, sess); out != "hi" {
		t.Fatalf("output = %q, want hi", out)
	}
	exit, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Code != 0 {
		t.Fatalf("exit = %+v, want code 0", exit)
	}
}

// TestTTYSessionStillWritable keeps the pty contract intact: the pty
// master is always writable, so Stdin is true whatever the spec said,
// and CloseInput stays NotAvailable.
func TestTTYSessionStillWritable(t *testing.T) {
	skipOnWindows(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	sess, err := local.New(t.TempDir()).Start(ctx, sandbox.SessionSpec{
		Argv: []string{"sh"},
		TTY:  true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	if !sess.Capabilities().Stdin {
		t.Error("Capabilities().Stdin = false, want true for a pty session")
	}
	if err := sess.CloseInput(); !errdefs.IsNotAvailable(err) {
		t.Fatalf("CloseInput error = %v, want NotAvailable", err)
	}
}
