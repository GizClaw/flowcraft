package stages

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// BuildHits converts ranked ContextItems into Hits and optionally
// runs the reranker (legacy runRecall order: build then rerank).
type BuildHits struct {
	reranker port.Reranker
}

// NewBuildHits constructs a BuildHits stage. reranker may be nil.
func NewBuildHits(reranker port.Reranker) *BuildHits {
	return &BuildHits{reranker: reranker}
}

// Name implements pipeline.Stage.
func (BuildHits) Name() string { return "build_hits" }

// Run implements pipeline.Stage.
func (s *BuildHits) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	hits := hitsFromItems(state.Ranked)
	state.Hits = hits
	detail := diagnostic.BuildHitsDetail{
		Count:      len(hits),
		InputCount: len(hits),
		Input:      candidateSnapshotPtr(hitSnapshots(hits)),
		Hits:       candidateSnapshotPtr(hitSnapshots(hits)),
	}
	if s.reranker != nil && len(hits) > 0 {
		reranked, err := s.reranker.Rerank(ctx, state.Query.Text, hits)
		if err != nil {
			detail.RerankErr = err.Error()
		} else {
			hits = reranked
			state.Hits = hits
			detail.Reranked = len(hits)
			detail.RerankedHits = candidateSnapshotPtr(hitSnapshots(hits))
		}
		if state.Plan != nil && state.Plan.TotalCap > 0 && len(hits) > state.Plan.TotalCap {
			hits = hits[:state.Plan.TotalCap]
			state.Hits = hits
		}
		detail.Count = len(hits)
		detail.Hits = candidateSnapshotPtr(hitSnapshots(hits))
	}
	return detail, nil
}

func hitsFromItems(items []domain.ContextItem) []domain.Hit {
	hits := make([]domain.Hit, 0, len(items))
	for _, it := range items {
		hits = append(hits, domain.Hit{
			Fact:    it.Fact,
			Score:   it.Candidate.Score,
			Sources: hitSources(it.Candidate),
		})
	}
	return hits
}

func hitSources(c domain.Candidate) []string {
	if c.Metadata != nil {
		if existing, ok := c.Metadata["sources"].([]string); ok && len(existing) > 0 {
			out := make([]string, len(existing))
			copy(out, existing)
			return out
		}
	}
	if c.Source != "" {
		return []string{c.Source}
	}
	return nil
}

var _ pipeline.Stage[*read.ReadState] = (*BuildHits)(nil)
