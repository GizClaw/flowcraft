package sandbox_test

import (
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/policy"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// These benchmarks guard the two hot paths a command takes before it runs. Neither
// is about absolute speed: they exist so an allocation added later is visible in
// the diff instead of in a p99 nobody attributes to the sandbox.

// BenchmarkConfineNarrow is the per-call budget resolution. It should stay
// allocation-free: it reads four fields, compares them and writes a value back.
func BenchmarkConfineNarrow(b *testing.B) {
	confine := sandbox.Confine{Resources: policy.Resources{
		CPUMillicores:  500,
		MemoryBytes:    64 << 20,
		MaxOutputBytes: 1 << 20,
	}}
	opts := sandbox.ExecOptions{
		Timeout:   time.Second,
		Resources: policy.Resources{MemoryBytes: 32 << 20},
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := confine.Narrow(opts); err != nil {
			b.Fatalf("Narrow: %v", err)
		}
	}
}

// BenchmarkShellSpecArgv is one command line built per call. It allocates one
// slice — the argv the process is started with — and nothing else.
func BenchmarkShellSpecArgv(b *testing.B) {
	spec := sandbox.ShellSpec{Name: "sh", Path: "/bin/sh", Args: []string{"-c"}}
	b.ReportAllocs()
	for b.Loop() {
		_ = spec.Argv("echo hello")
	}
}

// BenchmarkExecDrain measures the derived one-shot form: reading a ring's chunks
// and splitting them into two streams. The session is scripted, so this is the
// cost of the derivation itself, not of any particular backend.
func BenchmarkExecDrain(b *testing.B) {
	const chunks = 64
	script := make([]sandbox.OutputChunk, chunks)
	for i := range script {
		script[i] = chunk(int64(i), sandbox.SessionStreamStdout, "0123456789abcdef")
	}
	b.ReportAllocs()
	for b.Loop() {
		starter := &scriptedStarter{session: &scriptedSession{chunks: script}}
		if _, err := sandbox.Exec(b.Context(), starter, "x", nil, sandbox.ExecOptions{}); err != nil {
			b.Fatalf("Exec: %v", err)
		}
	}
}

// The guards below are what actually gates: a benchmark reports, a test fails.
// They live here rather than in CI's bench lane because `make bench-check` runs one
// iteration and compares nothing (see the Makefile), and because an allocation count
// is deterministic where a nanosecond is not.

var narrowSink sandbox.ExecOptions

// TestNarrowDoesNotAllocate keeps the per-call budget resolution free of allocation:
// it runs once for every command any tool issues, and it is four field comparisons.
func TestNarrowDoesNotAllocate(t *testing.T) {
	confine := sandbox.Confine{Resources: policy.Resources{
		CPUMillicores:  500,
		MemoryBytes:    64 << 20,
		MaxOutputBytes: 1 << 20,
	}}
	opts := sandbox.ExecOptions{Timeout: time.Second, Resources: policy.Resources{MemoryBytes: 32 << 20}}

	allocs := testing.AllocsPerRun(100, func() {
		resolved, err := confine.Narrow(opts)
		if err != nil {
			t.Fatalf("Narrow: %v", err)
		}
		narrowSink = resolved
	})
	if allocs != 0 {
		t.Fatalf("Narrow allocates %.0f times per call; it runs for every command", allocs)
	}
}

var argvSink []string

// TestShellSpecArgvAllocatesOnce bounds the command line build: one slice, the argv
// itself. More than that means the prefix is being copied per command.
func TestShellSpecArgvAllocatesOnce(t *testing.T) {
	spec := sandbox.ShellSpec{Name: "sh", Path: "/bin/sh", Args: []string{"-c"}}
	allocs := testing.AllocsPerRun(100, func() { argvSink = spec.Argv("echo hello") })
	if allocs > 1 {
		t.Fatalf("Argv allocates %.0f times per call, want the argv slice only", allocs)
	}
}
