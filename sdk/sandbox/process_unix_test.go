//go:build unix

package sandbox_test

import (
	"context"
	"errors"
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
