package bm25

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// The pair below measures what the lane's kernel cache is worth: a
// search whose (identity, selector) pair is unchanged reuses the kernel,
// while alternating between two selectors forces a rebuild every time
// and reproduces the cost the cache removes.

func BenchmarkSearchKernelHit(b *testing.B) {
	for _, documents := range []int{400, 4000} {
		b.Run(fmt.Sprintf("documents=%d", documents), func(b *testing.B) {
			index, scope := benchProjection(b, documents)
			request := component.SearchRequest{
				Scope: scope, Query: "needle common", Limit: 8,
				Metadata: corememory.Metadata{"conversation_id": "c1"},
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := index.Search(context.Background(), request); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSearchKernelMiss(b *testing.B) {
	for _, documents := range []int{400, 4000} {
		b.Run(fmt.Sprintf("documents=%d", documents), func(b *testing.B) {
			index, scope := benchProjection(b, documents)
			requests := []component.SearchRequest{
				{Scope: scope, Query: "needle common", Limit: 8, Metadata: corememory.Metadata{"conversation_id": "c1"}},
				{Scope: scope, Query: "needle common", Limit: 8, Metadata: corememory.Metadata{"conversation_id": "c2"}},
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := index.Search(context.Background(), requests[i%len(requests)]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// benchProjection indexes documents facts split evenly across two
// conversation selectors, so both a hit and a miss path have a
// non-empty scored set.
func benchProjection(b *testing.B, documents int) (*Index, corememory.Scope) {
	b.Helper()
	workspaceDirectory := b.TempDir()
	ws, err := workspace.NewLocalWorkspace(workspaceDirectory)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ws.Close() })
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		b.Fatal(err)
	}
	index, err := New(Config{KV: kvStore, Projection: "facts"})
	if err != nil {
		b.Fatal(err)
	}
	artifacts := make([]component.Artifact, documents)
	for i := range artifacts {
		var text strings.Builder
		for j := 0; j < 30; j++ {
			fmt.Fprintf(&text, "w%d ", (i*7+j*13)%900)
		}
		value := artifact(fmt.Sprintf("item-%05d", i), "needle common "+text.String())
		if i%2 == 0 {
			value.Metadata["conversation_id"] = "c1"
		} else {
			value.Metadata["conversation_id"] = "c2"
		}
		artifacts[i] = value
	}
	scope := corememory.Scope{RuntimeID: "runtime"}
	if err := index.FullRebuild(context.Background(), component.ProjectionRequest{
		Scope: scope, Projection: "facts", Artifacts: artifacts,
	}); err != nil {
		b.Fatal(err)
	}
	return index, scope
}
