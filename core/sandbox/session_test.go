package sandbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	local "github.com/GizClaw/flowcraft/core/sandbox-local"
	"github.com/GizClaw/flowcraft/core/sandbox/sessionkit"
)

func TestRunnerCapabilities(t *testing.T) {
	r := local.New(t.TempDir())
	caps := r.Capabilities()

	if !caps.Policy.EnvAllowList {
		t.Error("EnvAllowList = false, want true")
	}
	if caps.Policy.MemoryCap != sessionkit.GroupCapsAvailable() {
		t.Errorf("MemoryCap = %v, want GroupCapsAvailable() = %v",
			caps.Policy.MemoryCap, sessionkit.GroupCapsAvailable())
	}
	if caps.Policy.CPUCap != sessionkit.GroupCapsAvailable() {
		t.Errorf("CPUCap = %v, want GroupCapsAvailable() = %v",
			caps.Policy.CPUCap, sessionkit.GroupCapsAvailable())
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
