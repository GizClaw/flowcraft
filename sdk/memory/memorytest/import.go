package memorytest

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/memory"
)

// ImportSuite drives the documented Import contract. The
// kernel-level guarantees are scope validation and
// Source-required; per-impl behaviour (chunk count, dataset
// partitioning, embedding model) is impl-specific and lives
// in the impl's own tests.
type ImportSuite struct {
	Spec         memory.Spec
	BuildRuntime func(t *testing.T) *memory.Runtime

	SampleScope memory.Scope
}

func RunImport(t *testing.T, s ImportSuite) {
	t.Helper()
	if s.BuildRuntime == nil {
		t.Fatal("ImportSuite.BuildRuntime is required")
	}
	if s.SampleScope.RuntimeID == "" {
		t.Fatal("ImportSuite.SampleScope.RuntimeID is required")
	}

	ctx := withCtx(t, defaultTestTimeout)

	t.Run("returns_document_id", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		resp, err := rt.ExecuteImport(ctx, memory.ImportRequest{
			Scope:     s.SampleScope,
			DatasetID: "kb",
			Source:    "memory://seed.md",
		})
		if err != nil {
			t.Fatalf("ExecuteImport: %v", err)
		}
		if resp.DocumentID == "" {
			t.Error("DocumentID is empty")
		}
	})

	t.Run("chunk_count_non_negative", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		resp, err := rt.ExecuteImport(ctx, memory.ImportRequest{
			Scope:     s.SampleScope,
			DatasetID: "kb",
			Source:    "memory://seed.md",
			ChunkPolicy: memory.ChunkPolicy{
				Target:    128,
				Splitter:  "fixed",
				Tokenizer: "cl100k_base",
			},
		})
		if err != nil {
			t.Fatalf("ExecuteImport: %v", err)
		}
		if resp.ChunkCount < 0 {
			t.Errorf("ChunkCount = %d, want >= 0", resp.ChunkCount)
		}
	})

	t.Run("empty_source_rejected", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		_, err := rt.ExecuteImport(ctx, memory.ImportRequest{
			Scope:     s.SampleScope,
			DatasetID: "kb",
		})
		memErr := memory.AsError(err)
		if memErr == nil || memErr.Kind != memory.KindInvalidRequest {
			t.Errorf("expected KindInvalidRequest, got: %v", err)
		}
	})
}
