package stages

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// IntentRoute selects the recall strategy and populates state.Intent.
type IntentRoute struct {
	router port.IntentRouter
}

func NewIntentRoute(router port.IntentRouter) *IntentRoute {
	return &IntentRoute{router: router}
}

func (IntentRoute) Name() string { return "intent_route" }

func (s *IntentRoute) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	started := time.Now()
	routed, err := s.router.Route(ctx, port.IntentRouterInput{
		Text:      state.Query.Text,
		Entities:  state.Query.Entities,
		Subject:   state.Query.Subject,
		Predicate: state.Query.Predicate,
		Object:    state.Query.Object,
		Kinds:     state.Query.Kinds,
		TimeRange: state.Query.TimeRange,
	})
	latency := time.Since(started)
	if err != nil {
		return diagnostic.IntentRouteDetail{
			QueryLen: len(state.Query.Text),
			Latency:  latency,
		}, err
	}
	limit := state.Query.Limit
	if limit <= 0 {
		limit = 10
	}
	route := routed.Route
	if route.Strategy == "" {
		route.Strategy = domain.RecallStrategyDefault
	}
	intent := &domain.QueryIntent{
		Text:      routed.Text,
		Entities:  routed.Entities,
		Subject:   routed.Subject,
		Predicate: routed.Predicate,
		Object:    routed.Object,
		Kinds:     append([]domain.FactKind(nil), routed.Kinds...),
		TimeRange: routed.TimeRange,
		Features:  routed.Features,
		Scope:     state.Scope,
		Limit:     limit,
		Route:     route,
	}
	state.Intent = intent
	kinds := make([]string, len(intent.Kinds))
	for i, k := range intent.Kinds {
		kinds[i] = string(k)
	}
	return diagnostic.IntentRouteDetail{
		QueryLen:                      len(intent.Text),
		Entities:                      intent.Entities,
		Kinds:                         kinds,
		Subject:                       intent.Subject,
		Predicate:                     intent.Predicate,
		Object:                        intent.Object,
		HasTimeRange:                  !intent.TimeRange.IsZero(),
		HasExplicitDate:               intent.Features.Temporal.HasExplicitDate,
		HasRelativeTemporalExpression: intent.Features.Temporal.HasRelativeExpression,
		TokenCount:                    len(intent.Features.Tokens),
		NumericCount:                  len(intent.Features.Numeric),
		QuotedCount:                   len(intent.Features.Quoted),
		ProperCount:                   len(intent.Features.Proper),
		Strategy:                      string(route.EffectiveStrategy()),
		Confidence:                    route.Confidence,
		Alternates:                    diagnosticIntentRouteAlternates(route.Alternates),
		Signals:                       append([]string(nil), route.Signals...),
		FallbackReason:                route.FallbackReason,
		Latency:                       latency,
	}, nil
}

func diagnosticIntentRouteAlternates(in []domain.IntentRouteCandidate) []diagnostic.IntentRouteCandidate {
	if len(in) == 0 {
		return nil
	}
	out := make([]diagnostic.IntentRouteCandidate, 0, len(in))
	for _, candidate := range in {
		out = append(out, diagnostic.IntentRouteCandidate{
			Strategy:   string(candidate.Strategy),
			Confidence: candidate.Confidence,
		})
	}
	return out
}

var _ pipeline.Stage[*read.ReadState] = (*IntentRoute)(nil)
