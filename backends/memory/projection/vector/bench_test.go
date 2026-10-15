package vector

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// BenchmarkSearchSnapshotScaling measures what an exact scan costs as one
// scope's table grows, and how much of that cost is the sort. It is the
// reading an approximate index would replace: the per-entry slope is the
// portable number, the absolute times belong to the machine that ran it.
//
// The vectors are synthetic, but the width is the eval model's, so the payload
// these sizes describe is real: an entry costs dimensions times four bytes.
func BenchmarkSearchSnapshotScaling(b *testing.B) {
	const dimensions = 2048
	sizes := []int{1000, 4000, 10000, 40000, 100000}
	query := randomVector(rand.New(rand.NewSource(7)), dimensions)
	for _, size := range sizes {
		build := randomSnapshot(rand.New(rand.NewSource(int64(size))), size, dimensions)
		b.Run(fmt.Sprintf("entries=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			var scan, score, sort time.Duration
			for i := 0; i < b.N; i++ {
				results, report, err := searchSnapshot(build, query, component.VectorSearchFilter{}, 0)
				if err != nil {
					b.Fatal(err)
				}
				if len(results) != size {
					b.Fatalf("scored %d of %d entries", len(results), size)
				}
				scan += report.scanNanos
				score += report.scoreNanos
				sort += report.sortNanos
			}
			b.ReportMetric(float64(scan.Nanoseconds())/float64(b.N)/1e6, "scan-ms")
			b.ReportMetric(float64(score.Nanoseconds())/float64(b.N)/1e6, "score-ms")
			b.ReportMetric(float64(sort.Nanoseconds())/float64(b.N)/1e6, "sort-ms")
		})
	}
}

// randomSnapshot builds a table of size entries with deterministic pseudo
// random vectors: the scan cost does not depend on the values, so the reading
// stays comparable between runs.
func randomSnapshot(source *rand.Rand, size, dimensions int) snapshot {
	build := snapshot{Dimensions: dimensions, Entries: make([]entry, size)}
	for i := range build.Entries {
		build.Entries[i] = entry{
			ID:            fmt.Sprintf("entry-%07d", i),
			Name:          "fact",
			Vector:        randomVector(source, dimensions),
			Source:        corememory.SourceRef{Kind: corememory.SourceMessage, ID: fmt.Sprintf("source-%d", i)},
			ContentDigest: fmt.Sprintf("digest-%d", i),
		}
	}
	return build
}

func randomVector(source *rand.Rand, dimensions int) []float32 {
	vector := make([]float32, dimensions)
	for i := range vector {
		vector[i] = source.Float32() - 0.5
	}
	return vector
}
