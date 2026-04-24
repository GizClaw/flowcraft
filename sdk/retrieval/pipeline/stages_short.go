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
		// Forward the caller's debug request so backends that honour
		// Capabilities.Debug can populate SearchResponse.Execution; the
		// pipeline copies that explanation into State for emission.
		Debug: st.Request.Debug,
	})
	if err != nil {
		return err
	}
	if resp != nil {
		st.Final = resp.Hits
		// Preserve the backend's structured explanation so the wrapping
		// pipeline can surface it on the short-circuit path. Without this
		// the pipeline would observe empty Recalls/Trace and produce a
		// nil SearchResponse.Execution even when the backend tried to
		// honour Debug.
		st.HybridExecution = resp.Execution
	}
	st.ShortCircuit = true
	return nil
}
