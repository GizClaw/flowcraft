package remote_test

import (
	"context"
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

// Output crosses the wire base64-encoded inside JSON; this is what a 64 KiB read
// costs to marshal and unmarshal.
func BenchmarkWireReadResponse(b *testing.B) {
	resp := wire.ReadResponse{Output: sandbox.SessionOutput{
		NextSeq: 64 << 10,
		Chunks: []sandbox.OutputChunk{{
			Seq:    0,
			Stream: sandbox.SessionStreamStdout,
			Data:   make([]byte, 64<<10),
		}},
	}}
	encoded, err := wire.Encode(resp)
	if err != nil {
		b.Fatalf("Encode: %v", err)
	}
	b.SetBytes(int64(len(encoded)))
	b.ResetTimer()
	for range b.N {
		var got wire.ReadResponse
		if err := wire.Decode(encoded, &got); err != nil {
			b.Fatalf("Decode: %v", err)
		}
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
