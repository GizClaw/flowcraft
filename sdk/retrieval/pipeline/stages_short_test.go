package pipeline

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

// nilHybridIndex advertises Capabilities.Hybrid = true and implements
// retrieval.Hybridable but returns (nil, nil) — emulating the pre-fix
// journal.Wrap bridge that #157 exposed. The short-circuit stage must
// NOT mark ShortCircuit=true in this case (issue #161), because the
// remaining LTM stages (SupersededDecay, SlotCollapse, TimeDecay,
// EntityBoost / EntityLinkBoost) carry the actual recall semantics
// for the in-memory backend.
type nilHybridIndex struct{ *memory.Index }

func (i *nilHybridIndex) Capabilities() retrieval.Capabilities {
	c := i.Index.Capabilities()
	c.Hybrid = true
	return c
}

func (i *nilHybridIndex) SearchHybrid(context.Context, string, retrieval.HybridRequest) (*retrieval.SearchResponse, error) {
	return nil, nil
}

// TestHybridShortCircuit_DoesNotShortCircuitOnNilResponse pins
// issue #161: when SearchHybrid returns (nil, nil) — historically
// the journal.Wrap bridge for non-Hybridable inners — the stage
// must leave ShortCircuit=false so the rest of the LTM pipeline
// still runs. Pre-fix the stage unconditionally set ShortCircuit
// even on nil resp, silently emptying Recall.
func TestHybridShortCircuit_DoesNotShortCircuitOnNilResponse(t *testing.T) {
	idx := &nilHybridIndex{Index: memory.New()}
	st := &State{
		Index:     idx,
		Namespace: "ns",
		Request: &retrieval.SearchRequest{
			QueryText: "q",
			TopK:      5,
		},
	}
	if err := (HybridShortCircuit{}).Run(context.Background(), st); err != nil {
		t.Fatalf("HybridShortCircuit.Run: %v", err)
	}
	if st.ShortCircuit {
		t.Fatalf("#161 regression: HybridShortCircuit must NOT short-circuit when SearchHybrid returns (nil, nil)")
	}
	if st.Final != nil {
		t.Fatalf("Final should be untouched on nil response; got %+v", st.Final)
	}
}
