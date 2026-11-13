package remote_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-local"
	"github.com/GizClaw/flowcraft/core/sandbox-remote"
	"github.com/GizClaw/flowcraft/core/sandbox-remote/wire"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// The protocol path's cost, and the one number that decides whether the wire
// design is reasonable: how much slower a command is when the boundary is behind
// a connection instead of in this process.

func BenchmarkWireEncodeSpec(b *testing.B) {
	spec := wire.FromSpec(richSpec())
	b.ResetTimer()
	for range b.N {
		if _, err := wire.Encode(spec); err != nil {
			b.Fatalf("Encode: %v", err)
		}
	}
}

func BenchmarkWireDecodeSpec(b *testing.B) {
	encoded, err := wire.Encode(wire.FromSpec(richSpec()))
	if err != nil {
		b.Fatalf("Encode: %v", err)
	}
	b.SetBytes(int64(len(encoded)))
	b.ResetTimer()
	for range b.N {
		var spec wire.Spec
		if err := wire.Decode(encoded, &spec); err != nil {
			b.Fatalf("Decode: %v", err)
		}
	}
}

// The read frame: one header, raw chunk bytes, no base64 and no JSON. Encode and
// decode are separate because they are separate costs at each end.
func BenchmarkReadFrame(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20} {
		out := sandbox.SessionOutput{
			NextSeq: int64(size),
			Chunks: []sandbox.OutputChunk{{
				Seq:    0,
				Stream: sandbox.SessionStreamStdout,
				Data:   make([]byte, size),
			}},
		}
		body := wire.AppendReadResponse(nil, out, false)

		b.Run(fmt.Sprintf("encode/%dK", size>>10), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			for range b.N {
				if got := wire.AppendReadResponse(nil, out, false); len(got) != len(body) {
					b.Fatalf("encoded %d bytes, want %d", len(got), len(body))
				}
			}
		})
		b.Run(fmt.Sprintf("decode/%dK", size>>10), func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			for range b.N {
				if _, _, err := wire.DecodeReadResponse(body); err != nil {
					b.Fatalf("DecodeReadResponse: %v", err)
				}
			}
		})
	}
}

func BenchmarkErrorEnvelope(b *testing.B) {
	err := errdefs.PolicyDeniedf("command %q is not allowed", "rm")
	b.ResetTimer()
	for range b.N {
		envelope := wire.FromError(err)
		if rebuilt := envelope.Err(); rebuilt == nil {
			b.Fatal("rebuilt error is nil")
		}
	}
}

// The comparison that matters. Both run the same command on the same host; the
// only difference is engine.Exec against a local runner versus the same runner
// behind a listener.
func BenchmarkExecEchoLocal(b *testing.B) {
	benchmarkEcho(b, localRunnerForBench(b))
}

func BenchmarkExecEchoRemote(b *testing.B) {
	benchmarkEcho(b, remoteRunnerForBench(b))
}

func benchmarkEcho(b *testing.B, runner sandbox.Runner) {
	b.ResetTimer()
	for range b.N {
		if _, err := engine.Exec(context.Background(), runner, "true", nil, sandbox.ExecOptions{}); err != nil {
			b.Fatalf("Exec: %v", err)
		}
	}
}

// TestRemoteOverheadRatio reports the wire's cost as a multiple rather than as an
// absolute number: absolute times belong to the machine, the ratio belongs to the
// design. It is not a gate — a regression shows up as a changed ratio in the
// benchmark output — but it is the number worth watching.
func TestRemoteOverheadRatio(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}

	localResult := testing.Benchmark(BenchmarkExecEchoLocal)
	remoteResult := testing.Benchmark(BenchmarkExecEchoRemote)

	localNs := float64(localResult.NsPerOp())
	remoteNs := float64(remoteResult.NsPerOp())
	if localNs <= 0 || remoteNs <= 0 {
		t.Fatalf("unusable measurements: local=%v remote=%v", localResult, remoteResult)
	}
	t.Logf("echo: local %.0f µs/op, remote %.0f µs/op, ratio %.2fx",
		localNs/1000, remoteNs/1000, remoteNs/localNs)
}

func richSpec() sandbox.SessionSpec {
	return sandbox.SessionSpec{
		ID:    "bench",
		Argv:  []string{"sh", "-c", "echo hello"},
		TTY:   true,
		Rows:  40,
		Cols:  120,
		Stdin: true,
		Opts: sandbox.ExecOptions{
			WorkDir:   "/work",
			Timeout:   time.Second,
			Env:       sandbox.EnvPolicy{Allow: []string{"PATH", "HOME"}, Inject: map[string]string{"K": "v"}},
			Net:       permission.NetPolicy{Mode: permission.NetAllowList, AllowHosts: []string{"example.com"}},
			Resources: sandbox.ResourceLimits{MemoryBytes: 1 << 20, CPUMillicores: 500, MaxOutputBytes: 4096},
		},
	}
}

func localRunnerForBench(b *testing.B) sandbox.Runner {
	b.Helper()
	runner := local.New(b.TempDir())
	b.Cleanup(func() { _ = runner.Close() })
	return runner
}

func remoteRunnerForBench(b *testing.B) sandbox.Runner {
	b.Helper()
	root := b.TempDir()

	inner := local.New(root)
	b.Cleanup(func() { _ = inner.Close() })

	server, err := remote.NewServer(inner, remote.WithServerToken(testToken))
	if err != nil {
		b.Fatalf("NewServer: %v", err)
	}
	b.Cleanup(func() { _ = server.Close() })
	listener := httptest.NewServer(server)
	b.Cleanup(listener.Close)

	client, err := remote.New(context.Background(), listener.URL, remote.WithToken(testToken))
	if err != nil {
		b.Fatalf("remote.New: %v", err)
	}
	b.Cleanup(func() { _ = client.Close() })
	return client
}

// Bulk output is the case the short-command ratio says nothing about: every read is
// a round trip, and the payload used to be base64 inside JSON. Both transports drain
// the same 1 MiB child so the only difference is where the bytes travel.
//
// Read the two columns differently since the read frame landed: the remote numbers
// are bytes in flight, while the local ones are a cursor walk over a retained log —
// a read is a view of the ring, so nothing is copied and the MB/s says how fast the
// session can be indexed, not how fast it can move memory.
func BenchmarkReadThroughput1M(b *testing.B) {
	cases := []struct {
		name   string
		runner func(*testing.B) sandbox.Runner
	}{
		{"local", localRunnerForBench},
		{"remote", remoteRunnerForBench},
	}
	// The window is the knob that matters over a network: each Read is one round
	// trip, so a 64 KiB window pays sixteen of them per megabyte.
	for _, window := range []int{64 << 10, 256 << 10, 1 << 20} {
		for _, tc := range cases {
			b.Run(fmt.Sprintf("%s/window=%dK", tc.name, window>>10), func(b *testing.B) {
				benchmarkDrain(b, tc.runner(b), window)
			})
		}
	}
}

func benchmarkDrain(b *testing.B, runner sandbox.Runner, window int) {
	b.Helper()
	ctx := context.Background()
	sess, err := runner.Start(ctx, sandbox.SessionSpec{
		Argv: []string{"dd", "if=/dev/zero", "bs=1048576", "count=1"},
	})
	if err != nil {
		b.Fatalf("Start: %v", err)
	}
	defer func() { _ = sess.Close() }()

	// The first drain absorbs the spawn and any short read; every measured
	// iteration re-reads a complete log from cursor 0.
	if n := drainAll(b, ctx, sess, 0, window); n != 1<<20 {
		b.Fatalf("first drain read %d bytes, want %d", n, 1<<20)
	}

	b.SetBytes(1 << 20)
	b.ResetTimer()
	for range b.N {
		if n := drainAll(b, ctx, sess, 0, window); n != 1<<20 {
			b.Fatalf("drain read %d bytes", n)
		}
	}
}

func drainAll(b *testing.B, ctx context.Context, sess sandbox.Session, after int64, window int) int {
	b.Helper()
	total := 0
	for {
		out, err := sess.Read(ctx, after, window)
		if err != nil {
			b.Fatalf("Read: %v", err)
		}
		for _, c := range out.Chunks {
			if c.Stream == sandbox.SessionStreamStdout {
				total += len(c.Data)
			}
		}
		after = out.NextSeq
		if out.EOF {
			return total
		}
	}
}

// TestRemoteReadRatio reports the bulk-output multiplier as a number rather than
// an opinion: a same-host command's 1.17x says nothing about streaming bytes, and
// the read window is what decides it.
func TestRemoteReadRatio(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	measure := func(build func(*testing.B) sandbox.Runner, window int) (float64, float64) {
		result := testing.Benchmark(func(b *testing.B) { benchmarkDrain(b, build(b), window) })
		if result.NsPerOp() <= 0 {
			t.Fatalf("benchmark did not complete: %v", result)
		}
		ns := float64(result.NsPerOp())
		return ns / 1e6, float64(result.Bytes) / ns * 1e3 // ms, MB/s
	}
	for _, window := range []int{64 << 10, 1 << 20} {
		localMS, localMB := measure(localRunnerForBench, window)
		remoteMS, remoteMB := measure(remoteRunnerForBench, window)
		t.Logf("1 MiB drain at a %dK window: local %.2f ms (%.0f MB/s), remote %.2f ms (%.0f MB/s), slowdown %.1fx",
			window>>10, localMS, localMB, remoteMS, remoteMB, remoteMS/localMS)
	}
}

// The window engine.Exec reads with, measured through engine.Exec itself rather
// than through a hand-driven drain: this is the path a tool call takes.
func BenchmarkExec1MOutput(b *testing.B) {
	cases := []struct {
		name   string
		runner func(*testing.B) sandbox.Runner
	}{
		{"local", localRunnerForBench},
		{"remote", remoteRunnerForBench},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			runner := tc.runner(b)
			argv := []string{"dd", "if=/dev/zero", "bs=1048576", "count=1"}
			b.SetBytes(1 << 20)
			b.ResetTimer()
			for range b.N {
				result, err := engine.Exec(context.Background(), runner, argv[0], argv[1:], sandbox.ExecOptions{})
				if err != nil {
					b.Fatalf("Exec: %v", err)
				}
				if len(result.Stdout) != 1<<20 {
					b.Fatalf("stdout is %d bytes", len(result.Stdout))
				}
			}
		})
	}
}

// The write direction has the same encoding the read direction just lost: a large
// Stdin is base64 inside JSON on its way to the server.
func BenchmarkExec1MStdin(b *testing.B) {
	cases := []struct {
		name   string
		runner func(*testing.B) sandbox.Runner
	}{
		{"local", localRunnerForBench},
		{"remote", remoteRunnerForBench},
	}
	stdin := make([]byte, 1<<20)
	for i := range stdin {
		stdin[i] = byte('a' + i%26)
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			runner := tc.runner(b)
			b.SetBytes(1 << 20)
			b.ResetTimer()
			for range b.N {
				// cat echoes stdin back: the measurement covers both directions.
				result, err := engine.Exec(context.Background(), runner, "cat", nil,
					sandbox.ExecOptions{Stdin: stdin})
				if err != nil {
					b.Fatalf("Exec: %v", err)
				}
				if len(result.Stdout) != 1<<20 {
					b.Fatalf("stdout is %d bytes", len(result.Stdout))
				}
			}
		})
	}
}
