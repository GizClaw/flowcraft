package memorytest

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/memory"
)

// RecallSuite drives the documented Recall contract: TopK
// bounds the result count, Score is sorted descending, Source
// is opaque (impls may use any string shape), MinScore filters.
//
// The suite does not pre-populate any specific record set; an
// impl is expected to seed its long-term store with at least
// one hit per sub-test, and verify the kernel-level ordering
// and limits hold.
type RecallSuite struct {
	Spec         memory.Spec
	BuildRuntime func(t *testing.T) *memory.Runtime

	SampleScope memory.Scope
}

func RunRecall(t *testing.T, s RecallSuite) {
	t.Helper()
	if s.BuildRuntime == nil {
		t.Fatal("RecallSuite.BuildRuntime is required")
	}
	if s.SampleScope.RuntimeID == "" {
		t.Fatal("RecallSuite.SampleScope.RuntimeID is required")
	}

	ctx := withCtx(t, defaultTestTimeout)

	t.Run("top_k_bounds_result", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		// An impl that cannot return at least TopK hits
		// fails this test; that is a faithful signal that
		// the impl does not honour the kernel contract.
		const want = 3
		resp, err := rt.ExecuteRecall(ctx, memory.RecallRequest{
			Scope: s.SampleScope,
			Query: "seed",
			TopK:  want,
		})
		if err != nil {
			t.Fatalf("ExecuteRecall: %v", err)
		}
		if len(resp.Hits) != want {
			t.Errorf("len(Hits) = %d, want exactly %d seeded hits", len(resp.Hits), want)
		}
	})

	t.Run("hits_sorted_by_score_descending", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		resp, err := rt.ExecuteRecall(ctx, memory.RecallRequest{
			Scope: s.SampleScope,
			Query: "seed",
			TopK:  50,
		})
		if err != nil {
			t.Fatalf("ExecuteRecall: %v", err)
		}
		if len(resp.Hits) < 2 {
			t.Fatalf("len(Hits) = %d, need at least 2 to prove sorting", len(resp.Hits))
		}
		var prev float64
		for i, h := range resp.Hits {
			if i > 0 && h.Score > prev {
				t.Errorf("Hits[%d].Score = %f > prev %f (must be descending)",
					i, h.Score, prev)
			}
			prev = h.Score
		}
	})

	t.Run("min_score_filters_low_relevance", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		const threshold = 0.5
		resp, err := rt.ExecuteRecall(ctx, memory.RecallRequest{
			Scope:    s.SampleScope,
			Query:    "seed",
			TopK:     50,
			MinScore: threshold,
		})
		if err != nil {
			t.Fatalf("ExecuteRecall: %v", err)
		}
		if len(resp.Hits) == 0 {
			t.Fatal("MinScore query returned no hits; filter assertion would be vacuous")
		}
		for i, h := range resp.Hits {
			if h.Score < threshold {
				t.Errorf("Hits[%d].Score = %f, want >= %f (MinScore)",
					i, h.Score, threshold)
			}
		}
	})

	t.Run("source_is_opaque_string", func(t *testing.T) {
		// The kernel treats Source as opaque; this sub-test
		// only asserts the impl returns a non-empty Source
		// for a seeded hit (so callers can render it).
		rt := s.BuildRuntime(t)
		resp, err := rt.ExecuteRecall(ctx, memory.RecallRequest{
			Scope: s.SampleScope,
			Query: "seed",
			TopK:  5,
		})
		if err != nil {
			t.Fatalf("ExecuteRecall: %v", err)
		}
		if len(resp.Hits) == 0 {
			t.Fatal("seeded Recall returned no hits")
		}
		for i, h := range resp.Hits {
			if h.Source == "" {
				t.Errorf("Hits[%d].Source is empty", i)
			}
		}
	})

	t.Run("parts_reuse_inference_part", func(t *testing.T) {
		rt := s.BuildRuntime(t)
		resp, err := rt.ExecuteRecall(ctx, memory.RecallRequest{
			Scope: s.SampleScope,
			Query: "seed",
			TopK:  5,
		})
		if err != nil {
			t.Fatalf("ExecuteRecall: %v", err)
		}
		if len(resp.Hits) == 0 {
			t.Fatal("seeded Recall returned no hits")
		}
		for i, h := range resp.Hits {
			for j, p := range h.Parts {
				if _, ok := p.(inference.Part); !ok {
					t.Errorf("Hits[%d].Parts[%d] is %T, want inference.Part",
						i, j, p)
				}
			}
		}
	})
}
