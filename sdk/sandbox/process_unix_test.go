//go:build unix

package sandbox_test

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func TestLocalProcess_PipeStreamsAndExitCode(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/sh", "-c", "printf OUT; printf ERR >&2; exit 7"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()

	out := readAll(t, proc, 5*time.Second)
	var stdout, stderr strings.Builder
	for _, ch := range out.Chunks {
		switch ch.Stream {
		case sandbox.ProcessStreamStdout:
			stdout.Write(ch.Data)
		case sandbox.ProcessStreamStderr:
			stderr.Write(ch.Data)
		default:
			t.Errorf("unexpected stream %v", ch.Stream)
		}
	}
	if stdout.String() != "OUT" {
		t.Errorf("stdout = %q, want OUT", stdout.String())
	}
	if stderr.String() != "ERR" {
		t.Errorf("stderr = %q, want ERR", stderr.String())
	}
	if !out.EOF {
		t.Fatal("Read should report EOF after the process exits and output drains")
	}

	exit, err := proc.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Reason != sandbox.ProcessExited || exit.Code != 7 {
		t.Fatalf("exit = %+v, want exited(7)", exit)
	}
}

func TestLocalProcess_TTYWriteResizeReadExit(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
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

	if err := proc.Write(ctx, []byte("stty size\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := readUntil(t, proc, ctx, "24 80")
	if !strings.Contains(got, "24 80") {
		t.Fatalf("default window missing: %q", got)
	}

	if err := proc.Resize(ctx, 40, 120); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := proc.Write(ctx, []byte("stty size\n")); err != nil {
		t.Fatalf("Write after resize: %v", err)
	}
	got = readUntil(t, proc, ctx, "40 120")
	if !strings.Contains(got, "40 120") {
		t.Fatalf("resized window missing: %q", got)
	}

	if err := proc.Write(ctx, []byte("exit\n")); err != nil {
		t.Fatalf("Write exit: %v", err)
	}
	exit, err := proc.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Code != 0 || exit.Reason != sandbox.ProcessExited {
		t.Fatalf("exit = %+v, want exited(0)", exit)
	}
}

func TestLocalProcess_ResizeRejectedWithoutTTY(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/sleep", "1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()
	if err := proc.Resize(context.Background(), 24, 80); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Resize on pipe session = %v, want NotAvailable", err)
	}
}

func TestLocalProcess_Timeout(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/sleep", "30"},
		Opts: sandbox.ExecOptions{Timeout: 150 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()

	exit, err := proc.Wait(context.Background())
	if !errdefs.IsTimeout(err) {
		t.Fatalf("Wait error = %v, want timeout", err)
	}
	if exit.Reason != sandbox.ProcessTimedOut {
		t.Fatalf("exit = %+v, want ProcessTimedOut", exit)
	}
}

func TestLocalProcessManager_RegistryLifecycle(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	ctx := context.Background()

	proc, err := runner.Start(ctx, sandbox.ProcessSpec{
		ID:   "known",
		Argv: []string{"/bin/sh", "-c", "sleep 30"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()

	if _, err := runner.Start(ctx, sandbox.ProcessSpec{ID: "known", Argv: []string{"/bin/true"}}); !errdefs.IsConflict(err) {
		t.Fatalf("duplicate ID error = %v, want Conflict", err)
	}

	infos, err := runner.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != "known" || !infos[0].Running || infos[0].PID <= 0 {
		t.Fatalf("infos = %+v, want one running 'known' session", infos)
	}

	if err := runner.Terminate(ctx, "missing"); !errdefs.IsNotFound(err) {
		t.Fatalf("Terminate unknown = %v, want NotFound", err)
	}
	if err := runner.Terminate(ctx, "known"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	exit, err := proc.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait after Terminate: %v", err)
	}
	if exit.Reason != sandbox.ProcessTerminated {
		t.Fatalf("exit = %+v, want ProcessTerminated", exit)
	}

	infos, err = runner.List(ctx)
	if err != nil {
		t.Fatalf("List after Terminate: %v", err)
	}
	if len(infos) != 1 || infos[0].Running || infos[0].Exit == nil {
		t.Fatalf("exited session must stay listed until Close: %+v", infos)
	}

	if err := proc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	infos, err = runner.List(ctx)
	if err != nil {
		t.Fatalf("List after Close: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("sessions after Close = %+v, want none", infos)
	}
}

func TestLocalProcessManager_GeneratedID(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/sleep", "1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()
	if proc.ID() == "" {
		t.Fatal("empty spec.ID must produce a manager-generated ID")
	}
	infos, err := runner.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 || infos[0].ID != proc.ID() {
		t.Fatalf("infos = %+v, want generated ID %q", infos, proc.ID())
	}
}

func TestLocalProcess_ReadAfterClose(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/usr/bin/true"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := proc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := proc.Read(context.Background(), 0, 16); !errors.Is(err, sandbox.ErrProcessClosed) {
		t.Fatalf("Read after Close = %v, want ErrProcessClosed", err)
	}
}

func TestStartSession_ResolvesID(t *testing.T) {
	// Direct (non-registry) use must still yield a stable ID: the
	// caller's ID when set, otherwise a generated one.
	proc, err := sandbox.StartSession(context.Background(),
		sandbox.ProcessSpec{ID: "custom", Argv: []string{"/usr/bin/true"}},
		exec.Command("/usr/bin/true"))
	if err != nil {
		t.Fatalf("StartSession(custom id): %v", err)
	}
	defer func() { _ = proc.Close() }()
	if proc.ID() != "custom" {
		t.Fatalf("ID = %q, want custom", proc.ID())
	}

	proc, err = sandbox.StartSession(context.Background(),
		sandbox.ProcessSpec{Argv: []string{"/usr/bin/true"}},
		exec.Command("/usr/bin/true"))
	if err != nil {
		t.Fatalf("StartSession(generated id): %v", err)
	}
	defer func() { _ = proc.Close() }()
	if proc.ID() == "" {
		t.Fatal("StartSession must generate an ID when spec.ID is empty")
	}
}

func TestLocalProcess_Signal_NonTTY(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/sh", "-c", `trap 'echo caught; exit 0' INT; echo ready; read x`},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	readUntil(t, proc, ctx, "ready")

	signaler, ok := sandbox.ProcessSignalerOf(proc)
	if !ok {
		t.Fatal("ProcessSignalerOf must find the signal capability on Start handles")
	}
	if err := signaler.Signal(ctx, sandbox.ProcessSignalInterrupt); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if got := readUntil(t, proc, ctx, "caught"); !strings.Contains(got, "caught") {
		t.Fatalf("trap output missing: %q", got)
	}
	exit, err := proc.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if exit.Code != 0 || exit.Reason != sandbox.ProcessExited {
		t.Fatalf("exit = %+v, want exited(0)", exit)
	}
}

func TestLocalProcess_Signal_TTY(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/sh", "-c", `trap 'echo caught; exit 0' INT; echo ready; read x`},
		TTY:  true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	readUntil(t, proc, ctx, "ready")

	signaler, _ := sandbox.ProcessSignalerOf(proc)
	if err := signaler.Signal(ctx, sandbox.ProcessSignalInterrupt); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	if got := readUntil(t, proc, ctx, "caught"); !strings.Contains(got, "caught") {
		t.Fatalf("VINTR did not reach the foreground process group: %q", got)
	}
	if exit, err := proc.Wait(ctx); err != nil || exit.Code != 0 {
		t.Fatalf("Wait = %+v, %v; want exited(0)", exit, err)
	}
}

func TestLocalProcess_Signal_AfterExit_ErrProcessClosed(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/usr/bin/true"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()
	if _, err := proc.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	signaler, ok := sandbox.ProcessSignalerOf(proc)
	if !ok {
		t.Fatal("ProcessSignalerOf must find the capability on Start handles")
	}
	if err := signaler.Signal(context.Background(), sandbox.ProcessSignalInterrupt); !errors.Is(err, sandbox.ErrProcessClosed) {
		t.Fatalf("Signal after exit = %v, want ErrProcessClosed", err)
	}
}

func TestLocalProcess_Watch_ReplayThenLive(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/bin/sh", "-c", "printf first; sleep 0.3; printf second"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = proc.Close() }()

	source, ok := sandbox.ProcessEventSourceOf(proc)
	if !ok {
		t.Fatal("ProcessEventSourceOf must find the capability on Start handles")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w, err := source.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer func() { _ = w.Close() }()

	done := make(chan []sandbox.ProcessEvent, 1)
	go func() {
		var got []sandbox.ProcessEvent
		for ev := range w.Events() {
			got = append(got, ev)
		}
		done <- got
	}()
	if _, err := proc.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	_ = w.Close()
	events := <-done

	var sb strings.Builder
	var exited *sandbox.ProcessExit
	var exitedSeq int64
	for _, ev := range events {
		switch ev.Type {
		case sandbox.ProcessEventOutput:
			sb.Write(ev.Data)
		case sandbox.ProcessEventExited:
			exited = ev.Exit
			exitedSeq = ev.Seq
		case sandbox.ProcessEventLag, sandbox.ProcessEventClosed:
			t.Fatalf("unexpected event %v", ev.Type)
		}
	}
	if sb.String() != "firstsecond" {
		t.Fatalf("streamed output = %q, want firstsecond", sb.String())
	}
	if exited == nil || exited.Code != 0 {
		t.Fatalf("Exited event = %+v, want exited(0)", exited)
	}
	// Seq alignment with the pull cursor: everything Read from 0 must
	// end at the Exited seq.
	out, err := proc.Read(ctx, 0, 4096)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if out.NextSeq != exitedSeq || out.NextSeq != int64(len("firstsecond")) {
		t.Fatalf("Read NextSeq = %d, Exited seq = %d, want %d", out.NextSeq, exitedSeq, len("firstsecond"))
	}
}

func TestLocalProcess_Watch_AfterClose_ErrProcessClosed(t *testing.T) {
	runner := sandbox.NewLocalRunner(t.TempDir())
	proc, err := runner.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"/usr/bin/true"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := proc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	source, ok := sandbox.ProcessEventSourceOf(proc)
	if !ok {
		t.Fatal("ProcessEventSourceOf must find the capability on Start handles")
	}
	if _, err := source.Watch(context.Background()); !errors.Is(err, sandbox.ErrProcessClosed) {
		t.Fatalf("Watch after Close = %v, want ErrProcessClosed", err)
	}
}

func readAll(t *testing.T, proc sandbox.Process, timeout time.Duration) sandbox.ProcessOutput {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var seq int64
	var out sandbox.ProcessOutput
	for {
		chunk, err := proc.Read(ctx, seq, 4096)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		out.Chunks = append(out.Chunks, chunk.Chunks...)
		seq = chunk.NextSeq
		if chunk.EOF {
			out.NextSeq = chunk.NextSeq
			out.EOF = true
			return out
		}
	}
}

func readUntil(t *testing.T, proc sandbox.Process, ctx context.Context, want string) string {
	t.Helper()
	var seq int64
	var sb strings.Builder
	for {
		out, err := proc.Read(ctx, seq, 4096)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		for _, ch := range out.Chunks {
			sb.Write(ch.Data)
		}
		seq = out.NextSeq
		if strings.Contains(sb.String(), want) {
			return sb.String()
		}
		if out.EOF {
			return sb.String()
		}
	}
}
