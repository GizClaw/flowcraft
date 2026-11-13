//go:build unix

package sessionkit

import (
	"context"
	"errors"
	"os/exec"
	"strings"
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
