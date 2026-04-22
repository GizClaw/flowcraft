package pipeline

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// HybridShortCircuit short-circuits the pipeline when the underlying Index
// reports Capabilities.Hybrid && implements retrieval.Hybridable
// . Reads: Index, Request. Writes: Final, ShortCircuit.
//
// Place at the top of the pipeline (before MultiRetrieve) to bypass client
// fusion when the backend can do hybrid natively.
type HybridShortCircuit struct {
	Mode  retrieval.HybridMode
	Param map[string]any
}

// Name implements Stage.
func (s HybridShortCircuit) Name() string { return "HybridShortCircuit" }

// Run implements Stage.
func (s HybridShortCircuit) Run(ctx context.Context, st *State) error {
	if st.Index == nil || st.Request == nil {
		return nil
	}
	caps := st.Index.Capabilities()
	if !caps.Hybrid {
		return nil
	}
	h, ok := st.Index.(retrieval.Hybridable)
	if !ok {
		return nil
	}
	mode := s.Mode
	if mode == "" {
		mode = st.Request.HybridMode
	}
	resp, err := h.SearchHybrid(ctx, st.Namespace, retrieval.HybridRequest{
		QueryText:   st.Request.QueryText,
		QueryVector: st.Request.QueryVector,
		Filter:      st.Request.Filter,
		TopK:        st.Request.TopK,
		Mode:        mode,
		Param:       s.Param,
	})
	if err != nil {
		return err
	}
	if resp != nil {
		st.Final = resp.Hits
	}
	st.ShortCircuit = true
	return nil
}
