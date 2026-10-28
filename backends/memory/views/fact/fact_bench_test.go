package fact

import (
	"context"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// BenchmarkDeriveFacts reads what a derivation pass pays: size new facts
// written into one empty generation, which is the workload the identity probe
// lives in. Run it with -benchtime=1x; facts/s is the reading that moves.
func BenchmarkDeriveFacts(b *testing.B) {
	for _, size := range []int{100, 400, 1600} {
		b.Run(fmt.Sprintf("%d-facts", size), func(b *testing.B) {
			for index := 0; index < b.N; index++ {
				b.StopTimer()
				store, closeStore := newBenchFactStore(b)
				ctx := context.Background()
				b.StartTimer()
				for seed := 0; seed < size; seed++ {
					if _, err := store.Add(ctx, factRequest(
						fmt.Sprintf("seed-%d", seed), fmt.Sprintf("seed fact %d", seed),
					)); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				closeStore()
			}
			b.ReportMetric(float64(size)/b.Elapsed().Seconds(), "facts/s")
		})
	}
}

// BenchmarkAddDistinctFact reads the probe every Add runs before it writes --
// "is this text already a fact of this generation" -- against a generation
// that already holds size facts. Seeding is outside the timer, so the reading
// is one Add into a corpus of that size; run it with -benchtime=1x, because a
// second iteration would seed the same corpus twice.
func BenchmarkAddDistinctFact(b *testing.B) {
	for _, size := range []int{100, 400, 1600} {
		b.Run(fmt.Sprintf("%d-facts", size), func(b *testing.B) {
			for index := 0; index < b.N; index++ {
				b.StopTimer()
				store, closeStore := newBenchFactStore(b)
				ctx := context.Background()
				for seed := 0; seed < size; seed++ {
					if _, err := store.Add(ctx, factRequest(
						fmt.Sprintf("seed-%d", seed), fmt.Sprintf("seed fact %d", seed),
					)); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				if _, err := store.Add(ctx, factRequest("probe", "probe fact")); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				closeStore()
			}
		})
	}
}

// BenchmarkAddExistingFact reads the same probe when the text is already
// stored: the identity is found and the add merges into it instead of
// writing a new fact.
func BenchmarkAddExistingFact(b *testing.B) {
	for _, size := range []int{100, 400, 1600} {
		b.Run(fmt.Sprintf("%d-facts", size), func(b *testing.B) {
			for index := 0; index < b.N; index++ {
				b.StopTimer()
				store, closeStore := newBenchFactStore(b)
				ctx := context.Background()
				for seed := 0; seed < size; seed++ {
					if _, err := store.Add(ctx, factRequest(
						fmt.Sprintf("seed-%d", seed), fmt.Sprintf("seed fact %d", seed),
					)); err != nil {
						b.Fatal(err)
					}
				}
				request := factRequest(fmt.Sprintf("seed-%d", size-1), fmt.Sprintf("seed fact %d", size-1))
				request.Provenance = []corememory.SourceRef{{
					Kind: corememory.SourceMessage, ID: fmt.Sprintf("message-%d", size), Revision: "1",
				}}
				b.StartTimer()
				if _, err := store.Add(ctx, request); err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				closeStore()
			}
		})
	}
}

func newBenchFactStore(b *testing.B) (*FactStore, func()) {
	b.Helper()
	ws, err := workspace.NewLocalWorkspace(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		b.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		b.Fatal(err)
	}
	store, err := NewFactStore(logStore, kvStore)
	if err != nil {
		b.Fatal(err)
	}
	return store, func() { _ = ws.Close() }
}
