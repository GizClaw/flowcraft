//go:build unix

package sessionkit

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
)

// The configured budget is what bounds the log — before and after the exit. A
// finished session keeps its capture readable; it does not shrink to a tail, which
// would hand an in-progress reader a sequence gap.
func TestFinishedLogKeepsTheConfiguredBudget(t *testing.T) {
	log := newOutputLog(8 << 20)
	chunk := make([]byte, 256<<10)
	for range 8 { // 2 MiB captured, well inside the budget
		log.append(sandbox.SessionStreamStdout, chunk)
	}
	log.finish(sandbox.SessionExit{})
	defer log.close()

	if log.total != 2<<20 {
		t.Fatalf("after exit the log retains %d bytes, want the whole 2 MiB capture", log.total)
	}
	out, err := log.read(context.Background(), 0, 2<<20)
	if err != nil {
		t.Fatalf("read after exit: %v", err)
	}
	if len(out.Chunks) == 0 {
		t.Fatal("read after exit returned nothing")
	}
}

// A budget smaller than the capture is enforced while the session writes, which
// is the only bound a caller can see or configure.
func TestFinishedLogRespectsASmallBudget(t *testing.T) {
	const budget = 1 << 20
	log := newOutputLog(budget)
	chunk := make([]byte, 256<<10)
	for range 8 { // 2 MiB captured into a 1 MiB ring
		log.append(sandbox.SessionStreamStdout, chunk)
	}
	log.finish(sandbox.SessionExit{})
	defer log.close()

	if log.total > budget {
		t.Fatalf("retained %d bytes with a %d budget", log.total, budget)
	}
	if _, err := log.read(context.Background(), 0, 64); !errors.Is(err, sandbox.ErrSequenceGap) {
		t.Fatalf("read of a dropped cursor = %v, want ErrSequenceGap", err)
	}
	// The retained window still serves.
	if _, err := log.read(context.Background(), log.retainedSeqLocked(), 64<<10); err != nil {
		t.Fatalf("read of the retained window: %v", err)
	}
}

// Output has to stay readable after the process exits: the contract says so
// ("output remains readable until Close, including after Wait"), and a caller
// that reads slowly — or starts after the exit — must still get every byte the
// configured budget retains. Trimming to a smaller window at exit breaks that,
// and it surfaces as a sequence gap in the middle of an otherwise complete drain.
func TestOutputStaysReadableAfterExit(t *testing.T) {
	const budget = 4 << 20
	const total = 1 << 20

	log := newOutputLog(budget)
	chunk := make([]byte, 256<<10)
	for range total / len(chunk) {
		log.append(sandbox.SessionStreamStdout, chunk)
	}
	log.finish(sandbox.SessionExit{Code: 0})
	defer log.close()

	out, err := log.read(context.Background(), 0, total)
	if err != nil {
		t.Fatalf("read after exit: %v (the entry is inside the configured budget)", err)
	}
	var got int
	for _, c := range out.Chunks {
		got += len(c.Data)
	}
	if got != total {
		t.Fatalf("read after exit returned %d bytes, want %d", got, total)
	}
}
