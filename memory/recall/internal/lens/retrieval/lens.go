package retrieval

import (
	"fmt"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/lens"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
)

// Lens wires the retrieval projection + source.
type Lens struct{}

func (Lens) Spec() planner.LensSpec {
	return planner.LensSpec{
		Name:     planner.SourceRetrieval,
		Weight:   planner.WeightRetrieval,
		Activate: func(domain.QueryIntent) bool { return true },
	}
}

func (Lens) Build(deps lens.Deps) (lens.Built, error) {
	var opts []Option
	if deps.Embedder != nil {
		opts = append(opts, WithEmbedder(deps.Embedder))
	}
	proj, err := New(deps.Index, opts...)
	if err != nil {
		return lens.Built{}, fmt.Errorf("retrieval projection: %w", err)
	}
	var srcOpts []SourceOption
	if deps.Embedder != nil {
		srcOpts = append(srcOpts, WithSourceEmbedder(deps.Embedder))
	}
	return lens.Built{
		Projection: proj,
		Source:     NewSource(deps.Index, srcOpts...),
	}, nil
}
