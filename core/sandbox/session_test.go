//go:build unix

package sandbox_test

import (
	"context"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/local"
)

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
