package workspace_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	wsindex "github.com/GizClaw/flowcraft/memory/retrieval/workspace"
)

// TestSearch_DoesNotRaceCompaction_RemoveAll pins issue #170. The
// pre-fix Search snapshotted the manifest under RLock, released
// the lock, then iterated and openSegmentReader'd the snapshotted
// refs. A concurrent compactor that swapped the manifest AND
// RemoveAll'd the merged source segment dirs in the meantime made
// Search fail with 'segment file not found' on ENOENT.
//
// We assert correctness rather than performance: every Search
// under contention must either return the expected hits OR an
// error path that does NOT carry the workspace-level missing-file
// signature. The fix is to hold RLock through the full segment
// scan, so compaction's Lock waits for in-flight Searches before
// it issues RemoveAll.
//
// Test scaffolding:
//   - Build many small flushed segments so compactor has merge
//     work to do.
//   - Fan out searchers in N goroutines that loop while
//     compactors run Compact() concurrently. Compaction triggers
//     the RemoveAll that the pre-fix race depended on.
//   - Fail the test on the first 'segment file not found' /
//     workspace not-found error.
func TestSearch_DoesNotRaceCompaction_RemoveAll(t *testing.T) {
	idx, _ := newIdx(t,
		wsindex.WithAutoCompact(false), // drive Compact() manually so the test is deterministic
		wsindex.WithCompactionMinSegments(3),
	)
	const segCount = 12
	makeFlushedSegments(t, idx, "ns", segCount)

	ctx := context.Background()
	// Verify the docs are searchable from a single-threaded baseline
	// before we put load on the index.
	if r, err := idx.Search(ctx, "ns", retrieval.SearchRequest{QueryText: "fox", TopK: 50}); err != nil || len(r.Hits) == 0 {
		t.Fatalf("baseline Search failed: err=%v hits=%d", err, len(r.Hits))
	}

	const (
		searchers     = 8
		searchesEach  = 64
		compactRounds = 16
	)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for s := 0; s < searchers; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < searchesEach; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_, err := idx.Search(ctx, "ns", retrieval.SearchRequest{
					QueryText: "fox",
					TopK:      20,
				})
				if err != nil {
					// Race signature is workspace-layer file-not-found
					// surfacing through openSegmentReader; fail loud
					// so the test pinpoints #170 if it ever regresses.
					if strings.Contains(err.Error(), "not found") ||
						strings.Contains(err.Error(), "ENOENT") {
						t.Errorf("#170 regression: Search hit ENOENT during compaction: %v", err)
					} else {
						t.Errorf("unexpected Search error: %v", err)
					}
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for r := 0; r < compactRounds; r++ {
			// Add more flushed segments per round so the picker has
			// fresh merge work each iteration.
			for i := 0; i < 3; i++ {
				d := retrieval.Doc{
					ID:       fmt.Sprintf("doc-round-%d-%d", r, i),
					Content:  fmt.Sprintf("round %d doc %d quick brown fox", r, i),
					Vector:   []float32{float32(r), float32(i), 0, 0},
					Metadata: map[string]any{"r": r},
				}
				if err := idx.Upsert(ctx, "ns", []retrieval.Doc{d}); err != nil {
					t.Errorf("Upsert: %v", err)
					return
				}
				if err := idx.Flush(ctx, "ns"); err != nil {
					t.Errorf("Flush: %v", err)
					return
				}
			}
			if err := idx.Compact(ctx, "ns"); err != nil {
				t.Errorf("Compact: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}
