package stages

import (
	"context"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/write"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// GraphDependencies preflights graph-backed fact shapes before canonical append.
// KindParameter must never be temporarily written without existing canonical
// observation/span support, so this stage runs after resolve and before append.
type GraphDependencies struct {
	observations port.ObservationStore
	links        port.LinkStore
}

func NewGraphDependencies(observations port.ObservationStore, links port.LinkStore) *GraphDependencies {
	return &GraphDependencies{observations: observations, links: links}
}

func (GraphDependencies) Name() string { return "graph_dependencies" }

func (s *GraphDependencies) Skip(_ context.Context, state *write.WriteState) (bool, diagnostic.StageDetail) {
	if state == nil || !hasGraphDependencyFacts(state.Resolution.Facts) {
		return true, diagnostic.GraphDependencyDetail{}
	}
	return false, nil
}

func (s *GraphDependencies) Run(ctx context.Context, state *write.WriteState) (diagnostic.StageDetail, error) {
	started := time.Now()
	detail := diagnostic.GraphDependencyDetail{
		Checked: countGraphDependencyFacts(state.Resolution.Facts),
	}
	if err := validateParameterGraphDependencies(ctx, s.observations, s.links, state.Scope, state.Resolution.Facts); err != nil {
		state.FailedStage = "graph_dependencies"
		detail.Latency = time.Since(started)
		detail.FailedReason = graphDependencyFailureReason(err)
		detail.MissingDependencies = detail.FailedReason == "missing_dependencies"
		return detail, err
	}
	detail.Latency = time.Since(started)
	return detail, nil
}

func countGraphDependencyFacts(facts []domain.TemporalFact) int {
	count := 0
	for _, fact := range facts {
		if fact.Kind == domain.KindParameter || (fact.Origin.Kind == domain.OriginKindSemanticDerivation && len(fact.EvidenceRefs) > 0) {
			count++
		}
	}
	return count
}

func graphDependencyFailureReason(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "graph_dependencies_missing"):
		return "missing_dependencies"
	case strings.Contains(msg, "graph_dependencies_unsupported"):
		return "unsupported_evidence"
	default:
		return "validation_failed"
	}
}

var (
	_ pipeline.Stage[*write.WriteState]       = (*GraphDependencies)(nil)
	_ pipeline.Conditional[*write.WriteState] = (*GraphDependencies)(nil)
)
