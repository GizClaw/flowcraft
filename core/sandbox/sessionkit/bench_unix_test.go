//go:build unix

package sessionkit

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// The output log is on the hot path of every session: each chunk a child writes
// is copied into it, and each Read walks it. These measure the two ends of that.

func BenchmarkOutputLogAppend(b *testing.B) {
	// A budget large enough that nothing is trimmed: this is the append cost,
	// not the retention policy.
	l := newOutputLog(int64(b.N) * 4096)
	defer l.close()
	chunk := make([]byte, 4096)

	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for range b.N {
		l.append(sandbox.SessionStreamStdout, chunk)
	}
}

// Retention is the cost of a small budget: every append past it drops a chunk.
func BenchmarkOutputLogAppendWithTrim(b *testing.B) {
	l := newOutputLog(64 << 10)
	defer l.close()
	chunk := make([]byte, 4096)

	b.SetBytes(int64(len(chunk)))
	b.ResetTimer()
	for range b.N {
		l.append(sandbox.SessionStreamStdout, chunk)
	}
}

// Read latency at the size Exec asks for: a 64 KiB window out of a retained
// megabyte. This is the per-call cost a long command's output pays.
func BenchmarkOutputLogRead64K(b *testing.B) {
	const window = 64 << 10
	l := newOutputLog(4 << 20)
	defer l.close()
	for range 1 << 20 / 4096 {
		l.append(sandbox.SessionStreamStdout, make([]byte, 4096))
	}

	ctx := context.Background()
	b.SetBytes(window)
	b.ResetTimer()
	for range b.N {
		out, err := l.read(ctx, 0, window)
		if err != nil {
			b.Fatalf("read: %v", err)
		}
		if out.NextSeq != window {
			b.Fatalf("NextSeq = %d, want %d", out.NextSeq, window)
		}
	}
}

// A drain of one megabyte at the 64 KiB window Exec uses: bytes per second for a
// command that produces a lot of output.
func BenchmarkOutputLogDrain1M(b *testing.B) {
	const total = 1 << 20
	l := newOutputLog(4 << 20)
	defer l.close()
	for range total / 4096 {
		l.append(sandbox.SessionStreamStdout, make([]byte, 4096))
	}

	ctx := context.Background()
	b.SetBytes(total)
	b.ResetTimer()
	for range b.N {
		after := int64(0)
		for after < total {
			out, err := l.read(ctx, after, 64<<10)
			if err != nil {
				b.Fatalf("read: %v", err)
			}
			after = out.NextSeq
		}
	}
}

// Subscribing replays the retained window; this is what a UI attaching to a busy
// session pays once.
func BenchmarkOutputLogWatchReplay(b *testing.B) {
	l := newOutputLog(4 << 20)
	defer l.close()
	for range 1 << 20 / 4096 {
		l.append(sandbox.SessionStreamStdout, make([]byte, 4096))
	}

	b.ResetTimer()
	for range b.N {
		w := l.subscribe()
		_ = w.Close()
		l.removeSubscriber(w)
	}
}
