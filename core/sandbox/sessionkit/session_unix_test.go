//go:build unix

package sessionkit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// These tests drive StartSession directly with a real child. The shared spawn
// path is the one piece every backend relies on and none of them should have to
// assert on its own, so its semantics are pinned here once: the /dev/null stdin
// rule, stream separation, the pty path, the exit classification and Watch's
// replay.

// startSh runs script through sh(1) under the shared spawn path.
func startSh(t *testing.T, spec sandbox.SessionSpec, script string) sandbox.Session {
	t.Helper()
	if spec.Argv == nil {
		spec.Argv = []string{"sh", "-c", script}
	}
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	sess, err := StartSession(context.Background(), spec, cmd)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// drain reads to EOF and returns per-stream output plus the exit.
func drain(t *testing.T, sess sandbox.Session) (map[sandbox.SessionStream]string, sandbox.SessionExit, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	streams := make(map[sandbox.SessionStream]string)
	after := int64(0)
	for {
		out, err := sess.Read(ctx, after, 4096)
		if err != nil {
			return streams, sandbox.SessionExit{}, err
		}
		for _, c := range out.Chunks {
			streams[c.Stream] += string(c.Data)
		}
		if out.NextSeq < after {
			t.Fatalf("NextSeq went backwards: %d after %d", out.NextSeq, after)
		}
		after = out.NextSeq
		if out.EOF {
			break
		}
	}
	exit, err := sess.Wait(ctx)
	return streams, exit, err
}

func TestStartSessionSeparatesStreams(t *testing.T) {
	sess := startSh(t, sandbox.SessionSpec{}, "printf out; printf err >&2")

	streams, exit, err := drain(t, sess)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := streams[sandbox.SessionStreamStdout]; got != "out" {
		t.Errorf("stdout = %q, want out", got)
	}
	if got := streams[sandbox.SessionStreamStderr]; got != "err" {
		t.Errorf("stderr = %q, want err", got)
	}
	if exit.Code != 0 || exit.Reason != sandbox.SessionExited {
		t.Errorf("exit = %+v, want a normal exit 0", exit)
	}

	// The log stays readable after the exit, and re-reading replays.
	ctx := context.Background()
	out, err := sess.Read(ctx, 0, 64)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	var replayed []byte
	for _, c := range out.Chunks {
		replayed = append(replayed, c.Data...)
	}
	if !strings.Contains(string(replayed), "out") {
		t.Errorf("replay = %q", replayed)
	}
}

// With no declared input the child gets /dev/null, so a program that reads stdin
// exits instead of blocking on a pipe nobody will close.
func TestStartSessionUndeclaredStdinIsNull(t *testing.T) {
	sess := startSh(t, sandbox.SessionSpec{}, "cat")

	if sess.Capabilities().Stdin {
		t.Error("session reports an input side the spec never declared")
	}
	if err := sess.Write(context.Background(), []byte("x")); !errdefs.IsNotAvailable(err) {
		t.Errorf("Write = %v, want NotAvailable", err)
	}
	if err := sess.CloseInput(); err != nil {
		t.Errorf("CloseInput = %v, want a no-op success", err)
	}

	streams, exit, err := drain(t, sess)
	if err != nil {
		t.Fatalf("drain: %v (the child is blocked on stdin, so fd 0 is not /dev/null)", err)
	}
	if streams[sandbox.SessionStreamStdout] != "" {
		t.Errorf("stdout = %q, want nothing", streams[sandbox.SessionStreamStdout])
	}
	if exit.Code != 0 {
		t.Errorf("exit = %+v", exit)
	}
}

func TestStartSessionStdinStreams(t *testing.T) {
	sess := startSh(t, sandbox.SessionSpec{Stdin: true}, "cat")
	if !sess.Capabilities().Stdin {
		t.Fatal("session does not report the declared input side")
	}
	if err := sess.Write(context.Background(), []byte("hello-stdin")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := sess.CloseInput(); err != nil {
		t.Fatalf("CloseInput: %v", err)
	}

	streams, exit, err := drain(t, sess)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := streams[sandbox.SessionStreamStdout]; got != "hello-stdin" {
		t.Fatalf("stdout = %q, want the written bytes", got)
	}
	if exit.Code != 0 {
		t.Errorf("exit = %+v", exit)
	}
}

// TestStartSessionWriteToClosedInput is a regression for the flake the Linux
// suite caught: a child that closes its stdin and keeps running — or exits after
// probing fd 0 — leaves the writer with EPIPE, which is the ordinary outcome of
// writing to a process that stopped reading, not an internal failure. The exit
// status is where the caller learns what happened.
func TestStartSessionWriteToClosedInput(t *testing.T) {
	sess := startSh(t, sandbox.SessionSpec{Stdin: true}, "exec 0<&-; sleep 5")
	if err := sess.Write(context.Background(), bytes.Repeat([]byte("x"), 1<<16)); err != nil {
		t.Fatalf("Write to a child that closed its input: %v", err)
	}
}

func TestStartSessionTTY(t *testing.T) {
	sess := startSh(t, sandbox.SessionSpec{TTY: true, Rows: 24, Cols: 80}, "printf tty-line")
	caps := sess.Capabilities()
	if !caps.TTY || !caps.Stdin {
		t.Fatalf("tty session capabilities = %+v", caps)
	}
	if err := sess.Resize(context.Background(), 40, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	// A pty merges the streams and has no separate stdin to close.
	if err := sess.CloseInput(); !errdefs.IsNotAvailable(err) {
		t.Errorf("CloseInput on a tty = %v, want NotAvailable", err)
	}

	streams, exit, err := drain(t, sess)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, separate := streams[sandbox.SessionStreamStdout]; separate {
		t.Error("a tty session reported a separate stdout stream")
	}
	if !strings.Contains(streams[sandbox.SessionStreamTTY], "tty-line") {
		t.Errorf("tty output = %q", streams[sandbox.SessionStreamTTY])
	}
	if exit.Code != 0 {
		t.Errorf("exit = %+v", exit)
	}
}

// A tty session has to keep the tail a child wrote just before it died: the pty
// buffer still holds those bytes when the child is reaped, and closing the master
// before the copier drains them discards the answer's last words. Under load that
// is an intermittently empty result.
func TestStartSessionTTYKeepsTheTailOfADyingChild(t *testing.T) {
	for range 20 {
		sess := startSh(t, sandbox.SessionSpec{TTY: true, Rows: 24, Cols: 200},
			"printf tty-tail; kill -9 $$")
		streams, exit, err := drain(t, sess)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if !strings.Contains(streams[sandbox.SessionStreamTTY], "tty-tail") {
			t.Fatalf("tty output = %q, want the tail the child wrote before dying",
				streams[sandbox.SessionStreamTTY])
		}
		// The reordering sits in front of the classification: a child killed by a
		// signal is still reported as signaled.
		if exit.Reason != sandbox.SessionSignaled || exit.Signal != int(syscall.SIGKILL) {
			t.Fatalf("exit = %+v, want it signalled by SIGKILL", exit)
		}
	}
}

// A backgrounded grandchild that ignores the hangup keeps the pty slave open after
// the child exits. Closing the master does not interrupt a read that is already
// blocked, so without the kill the copier would wait on a descriptor nobody will
// close and Wait would never return. The tail still arrives, and the call is
// bounded.
func TestStartSessionTTYReturnsWhileAGrandchildHoldsTheSlave(t *testing.T) {
	sess := startSh(t, sandbox.SessionSpec{TTY: true, Rows: 24, Cols: 200},
		`trap "" HUP; sleep 30 & printf tty-tail`)
	start := time.Now()
	streams, _, err := drain(t, sess)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	// The grace is 500 ms per step, so three steps plus the helper's own work is
	// well inside two seconds; anything near the helper's deadline means the drain
	// is not bounded.
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Errorf("drain took %s, want the drain bounded by ptyDrainGrace", elapsed)
	}
	// On Linux the ignored hangup keeps the slave open, so the drain has to spend
	// its grace and then kill the group: measuring that proves this test exercises
	// the bound instead of passing on a tty that closed by itself. macOS closes the
	// terminal with the session leader, so there the same child costs nothing and
	// the assertion would be false.
	if runtime.GOOS == "linux" && elapsed < ptyDrainGrace {
		t.Errorf("drain took %s, want at least the %s grace: the grandchild was gone, so this test proved nothing",
			elapsed, ptyDrainGrace)
	}
	if !strings.Contains(streams[sandbox.SessionStreamTTY], "tty-tail") {
		t.Errorf("tty output = %q, want the tail before the grandchild", streams[sandbox.SessionStreamTTY])
	}
}

// The ordering itself, without a child to race: the master must stay open while
// the copier works and close once it is done. A pipe stands in for the pty master
// because it reports the same two states — readable-with-deadline while open,
// os.ErrClosed after — and unlike a pty it honours a read deadline.
func TestDrainAndClosePTYClosesAfterTheCopier(t *testing.T) {
	master, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = writer.Close() }()
	pty := &ptyHandle{}
	pty.set(master)

	var copiers sync.WaitGroup
	copiers.Add(1)
	released := make(chan struct{})
	go func() {
		defer copiers.Done()
		<-released
	}()

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		drainAndClosePTY(&copiers, pty, syscall.Getpgrp(), "test")
	}()

	// While the copier is still working the master has to be open: this is the
	// window in which the child's last write is read.
	if err := master.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	buf := make([]byte, 1)
	_, readErr := master.Read(buf)
	if errors.Is(readErr, os.ErrClosed) {
		t.Fatal("the master was closed while the copier was still reading")
	}

	close(released)
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("the drain did not return after the copier finished")
	}
	// And once the drain returned, the master is closed — exactly one close, by
	// the function that owns the order.
	if _, err := master.Read(buf); !errors.Is(err, os.ErrClosed) {
		t.Errorf("master still readable after the drain: %v", err)
	}
}

// A timeout is a classification, not just an exit code: the caller has to be able
// to tell "it ran out of time" from "it failed".
func TestStartSessionTimeout(t *testing.T) {
	sess := startSh(t, sandbox.SessionSpec{
		Opts: sandbox.ExecOptions{Timeout: 50 * time.Millisecond},
	}, "sleep 30")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exit, err := sess.Wait(ctx)
	if err == nil {
		t.Fatal("Wait returned no error for a timed-out session")
	}
	if !errdefs.IsTimeout(err) {
		t.Errorf("err = %v, want a timeout classification", err)
	}
	if exit.Reason != sandbox.SessionTimedOut {
		t.Errorf("reason = %v, want timed out", exit.Reason)
	}
}

func TestStartSessionTerminate(t *testing.T) {
	sess := startSh(t, sandbox.SessionSpec{}, "sleep 30")

	if err := sess.Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	exit, err := sess.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Reason != sandbox.SessionTerminated {
		t.Errorf("reason = %v, want terminated", exit.Reason)
	}
	if err := sess.Terminate(context.Background()); err != nil {
		t.Errorf("second Terminate = %v, want nil", err)
	}
	// The output log is still readable after a kill.
	if _, err := sess.Read(context.Background(), 0, 16); err != nil && !errors.Is(err, sandbox.ErrSequenceGap) {
		t.Errorf("Read after Terminate = %v", err)
	}
}

// Watch replays what the session already produced and then follows live: the
// half that cannot be tested on the bare output log.
func TestStartSessionWatchReplayThenLive(t *testing.T) {
	sess := startSh(t, sandbox.SessionSpec{}, "printf one; sleep 0.1; printf two")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watcher, err := sess.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = watcher.Close() }()

	var output strings.Builder
	sawExit := false
	for !sawExit {
		select {
		case ev, open := <-watcher.Events():
			if !open {
				t.Fatal("event stream closed before the exit event")
			}
			switch ev.Type {
			case sandbox.SessionEventOutput:
				output.Write(ev.Data)
			case sandbox.SessionEventExited:
				sawExit = true
			case sandbox.SessionEventLag:
				t.Fatalf("the subscription lagged at seq %d with a consumer that never fell behind", ev.Seq)
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for the exit event")
		}
	}
	if got := output.String(); got != "onetwo" {
		t.Fatalf("streamed output = %q, want the replay plus the live line", got)
	}
}
