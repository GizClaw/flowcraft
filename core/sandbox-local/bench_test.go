//go:build unix

package local_test

import (
	"context"
	"os/exec"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-local"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// The process path's floor: how long one short command takes end to end through
// the public surface. Everything a tool does pays this before it does any work of
// its own.
func BenchmarkExecEcho(b *testing.B) {
	runner := local.New(b.TempDir())
	defer func() { _ = runner.Close() }()
	opts := sandbox.ExecOptions{}

	b.ResetTimer()
	for range b.N {
		if _, err := engine.Exec(context.Background(), runner, "true", nil, opts); err != nil {
			b.Fatalf("Exec: %v", err)
		}
	}
}

// Start is the session path's floor, measured to first use rather than to exit:
// interactive tools pay this and then keep the session.
func BenchmarkSessionStartLocal(b *testing.B) {
	runner := local.New(b.TempDir())
	defer func() { _ = runner.Close() }()

	b.ResetTimer()
	for range b.N {
		sess, err := runner.Start(context.Background(), sandbox.SessionSpec{Argv: []string{"true"}})
		if err != nil {
			b.Fatalf("Start: %v", err)
		}
		if _, err := sess.Wait(context.Background()); err != nil {
			b.Fatalf("Wait: %v", err)
		}
		_ = sess.Close()
	}
}

// Reading a real child's output at the window Exec uses. The numbers here are
// what a tool call pays per 64 KiB of command output, including the pipe copy.
func BenchmarkSessionReadThroughputLocal(b *testing.B) {
	runner := local.New(b.TempDir())
	defer func() { _ = runner.Close() }()

	b.SetBytes(1 << 20)
	b.ResetTimer()
	for range b.N {
		sess, err := runner.Start(context.Background(), sandbox.SessionSpec{
			// One 1 MiB write: the producer should not be the bottleneck, or
			// the number measures dd rather than the session's read path.
			Argv: []string{"dd", "if=/dev/zero", "bs=1048576", "count=1"},
		})
		if err != nil {
			b.Fatalf("Start: %v", err)
		}
		after := int64(0)
		for {
			out, err := sess.Read(context.Background(), after, 64<<10)
			if err != nil {
				b.Fatalf("Read: %v", err)
			}
			after = out.NextSeq
			if out.EOF {
				break
			}
		}
		_ = sess.Close()
	}
}

// A guard against the benchmark itself rotting: if the shell or dd is missing the
// numbers above would be zero-cost lies.
func TestBenchmarkToolingAvailable(t *testing.T) {
	for _, name := range []string{"sh", "dd", "tr", "true"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s is not available: %v", name, err)
		}
	}
}
