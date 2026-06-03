package stages

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

const (
	observationRecallSource      = "observation"
	observationRecallScanLimit   = 500
	observationRecallDefaultCap  = 4
	observationRecallStrictCap   = 2
	observationRecallMinScore    = 0.35
	observationRecallStrictScore = 0.65
	observationRecallUnderCount  = 3
	observationRecallMinOverlap  = 3
)

// ObservationRecall is the raw-evidence lane for the O/A/L architecture. It
// adds high-overlap observations as observation nodes so extractor misses can
// surface in recall without inventing assertion facts.
type ObservationRecall struct {
	observations port.ObservationStore
}

func NewObservationRecall(observations port.ObservationStore) *ObservationRecall {
	return &ObservationRecall{observations: observations}
}

func (ObservationRecall) Name() string { return "observation_recall" }

func (s *ObservationRecall) Skip(_ context.Context, state *read.ReadState) (bool, diagnostic.StageDetail) {
	read.PromoteMergedItems(state)
	if s == nil || s.observations == nil || state == nil || strings.TrimSpace(state.Query.Text) == "" {
		return true, diagnostic.ObservationRecallDetail{}
	}
	return false, nil
}

func (s *ObservationRecall) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	started := time.Now()
	if state == nil {
		return diagnostic.ObservationRecallDetail{}, nil
	}
	read.PromoteMergedItems(state)
	detail := diagnostic.ObservationRecallDetail{InputCount: len(state.MergedItems)}
	queryTokens := observationRecallQueryTokens(state)
	if len(queryTokens) == 0 {
		detail.OutputCount = len(state.MergedItems)
		detail.Latency = time.Since(started)
		return detail, nil
	}

	existing := observationRecallExisting(state.MergedItems)
	minScore := observationRecallMinScoreForState(state)
	var scored []observationScored
	for _, scope := range state.Scope.EffectiveFederation() {
		if err := ctx.Err(); err != nil {
			return detail, err
		}
		observations, err := s.observations.List(ctx, scope, port.ObservationListQuery{Limit: observationRecallScanLimit})
		if err != nil {
			if isContextError(err) {
				return detail, err
			}
			detail.Err = err.Error()
			continue
		}
		detail.ScannedObservations += len(observations)
		for _, obs := range observations {
			if _, ok := existing[obs.ID]; ok {
				continue
			}
			if observationRecallDuplicateText(existing, obs.Text) {
				continue
			}
			scoreText := strings.TrimSpace(strings.Join([]string{obs.Speaker, obs.Text}, " "))
			if !observationRecallAllowsLexicalRescue(state, scoreText) {
				continue
			}
			score := observationRecallScore(queryTokens, scoreText)
			if score < minScore {
				continue
			}
			scored = append(scored, observationScored{observation: obs, score: score})
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].observation.ID < scored[j].observation.ID
	})

	for _, candidate := range scored {
		if detail.AddedObservations >= observationRecallMaxAdds(state) {
			break
		}
		item := observationContextItem(candidate.observation, candidate.score)
		state.MergedItems = append(state.MergedItems, item)
		existing[candidate.observation.ID] = struct{}{}
		existing[observationRecallTextKey(candidate.observation.Text)] = struct{}{}
		detail.AddedObservations++
		detail.AddedObservationIDs = append(detail.AddedObservationIDs, candidate.observation.ID)
	}
	detail.OutputCount = len(state.MergedItems)
	detail.Latency = time.Since(started)
	if snapshotsEnabled(state) {
		detail.Items = candidateSnapshotPtr(contextItemSnapshots(state.MergedItems))
	}
	return detail, nil
}

type observationScored struct {
	observation domain.Observation
	score       float64
}

func observationRecallQueryTokens(state *read.ReadState) map[string]struct{} {
	if state != nil && state.Plan != nil && len(state.Plan.Intent.Features.Tokens) > 0 {
		return state.Plan.Intent.Features.Tokens
	}
	if state != nil && state.Intent != nil && len(state.Intent.Features.Tokens) > 0 {
		return state.Intent.Features.Tokens
	}
	if state == nil {
		return nil
	}
	return recallintent.TextTokenSet(state.Query.Text)
}

func observationRecallExisting(items []domain.ContextItem) map[string]struct{} {
	out := make(map[string]struct{}, len(items)*2)
	for _, item := range items {
		if item.Fact.ID != "" {
			out[item.Fact.ID] = struct{}{}
		}
		if item.Fact.Content != "" {
			out[observationRecallTextKey(item.Fact.Content)] = struct{}{}
		}
		for _, ref := range item.Fact.EvidenceRefs {
			if ref.ObservationID != "" {
				out[ref.ObservationID] = struct{}{}
			}
			if ref.SpanID != "" {
				out[ref.SpanID] = struct{}{}
			}
			if ref.ID != "" {
				out[ref.ID] = struct{}{}
			}
			if ref.Text != "" {
				out[observationRecallTextKey(ref.Text)] = struct{}{}
			}
		}
		for _, ref := range item.Evidence {
			if ref.ObservationID != "" {
				out[ref.ObservationID] = struct{}{}
			}
			if ref.SpanID != "" {
				out[ref.SpanID] = struct{}{}
			}
			if ref.ID != "" {
				out[ref.ID] = struct{}{}
			}
			if ref.Text != "" {
				out[observationRecallTextKey(ref.Text)] = struct{}{}
			}
		}
	}
	return out
}

func observationRecallDuplicateText(existing map[string]struct{}, text string) bool {
	if strings.TrimSpace(text) == "" {
		return true
	}
	_, ok := existing[observationRecallTextKey(text)]
	return ok
}

func observationRecallScore(queryTokens map[string]struct{}, text string) float64 {
	text = strings.TrimSpace(text)
	if text == "" || len(queryTokens) == 0 {
		return 0
	}
	textTokens := recallintent.TextTokenSet(text)
	overlap := 0
	for token := range queryTokens {
		if _, ok := textTokens[token]; ok {
			overlap++
		}
	}
	requiredOverlap := min(observationRecallMinOverlap, len(queryTokens))
	if overlap < requiredOverlap {
		return 0
	}
	return float64(overlap) / float64(len(queryTokens))
}

func observationRecallAllowsLexicalRescue(state *read.ReadState, text string) bool {
	if state == nil || len(state.MergedItems) < observationRecallUnderCount {
		return true
	}
	if !observationRecallExactEvidenceQuery(state) {
		return false
	}
	features := observationRecallFeatures(state)
	if features.IsZero() {
		return false
	}
	textTokens := recallintent.TextTokenSet(text)
	if tokenSetIntersects(features.Proper, textTokens) ||
		tokenSetIntersects(features.Numeric, textTokens) ||
		tokenSetIntersects(features.Quoted, textTokens) {
		return true
	}
	return false
}

func observationContextItem(obs domain.Observation, score float64) domain.ContextItem {
	ts := obs.ObservedAt
	if ts.IsZero() {
		ts = obs.ReceivedAt
	}
	ref := domain.EvidenceRef{
		ID:            obs.ID,
		ObservationID: obs.ID,
		MessageID:     obs.MessageID,
		Role:          obs.Role,
		Text:          obs.Text,
		Timestamp:     ts,
	}
	if span := observationPrimarySpan(obs); span.ID != "" {
		ref.SpanID = span.ID
		if span.Text != "" {
			ref.Text = span.Text
		}
	}
	return domain.ContextItem{
		Candidate: domain.Candidate{
			Kind:        domain.GraphNodeObservation,
			ID:          obs.ID,
			Scope:       obs.Scope,
			Source:      observationRecallSource,
			Score:       score,
			EvidenceIDs: []string{obs.ID},
			Metadata:    map[string]any{"sources": []string{observationRecallSource}},
		},
		Ref:         domain.CandidateRef{Kind: domain.GraphNodeObservation, ID: obs.ID, Scope: obs.Scope, Source: observationRecallSource, Score: score, EvidenceIDs: []string{obs.ID}},
		Observation: obs,
		Evidence:    []domain.EvidenceRef{ref},
	}
}

func observationPrimarySpan(obs domain.Observation) domain.ObservationSpan {
	for _, span := range obs.Spans {
		if span.Text != "" && (span.Text == obs.Text || span.Kind == domain.ObservationSpanKindText) {
			return span
		}
	}
	if len(obs.Spans) > 0 {
		return obs.Spans[0]
	}
	return domain.ObservationSpan{}
}

func observationRecallTextKey(text string) string {
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

func observationRecallMaxAdds(state *read.ReadState) int {
	if state != nil && len(state.MergedItems) >= observationRecallUnderCount {
		if observationRecallExactEvidenceQuery(state) {
			return observationRecallStrictCap
		}
		return 1
	}
	if state != nil && state.Plan != nil && state.Plan.TotalCap > 0 {
		return min(observationRecallDefaultCap, max(1, state.Plan.TotalCap/4))
	}
	return observationRecallDefaultCap
}

func observationRecallExactEvidenceQuery(state *read.ReadState) bool {
	if state == nil {
		return false
	}
	features := observationRecallFeatures(state)
	route := domain.IntentRoute{}
	if state.Plan != nil {
		route = state.Plan.Intent.Route
	} else if state.Intent != nil {
		route = state.Intent.Route
	}
	return observationRecallExactEvidenceRoute(route) ||
		features.HasTimeSignal() || len(features.Numeric) > 0 || len(features.Quoted) > 0
}

func observationRecallFeatures(state *read.ReadState) domain.QueryFeatures {
	if state != nil && state.Plan != nil {
		return state.Plan.Intent.Features
	}
	if state != nil && state.Intent != nil {
		return state.Intent.Features
	}
	return domain.QueryFeatures{}
}

func tokenSetIntersects(a, b map[string]struct{}) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	for token := range a {
		if _, ok := b[token]; ok {
			return true
		}
	}
	return false
}

func observationRecallExactEvidenceRoute(route domain.IntentRoute) bool {
	switch route.EffectiveStrategy() {
	case domain.RecallStrategyTemporal, domain.RecallStrategyCount, domain.RecallStrategySet:
		return true
	default:
		return false
	}
}

func observationRecallMinScoreForState(state *read.ReadState) float64 {
	if state != nil && len(state.MergedItems) >= observationRecallUnderCount {
		return observationRecallStrictScore
	}
	return observationRecallMinScore
}

var (
	_ pipeline.Stage[*read.ReadState]       = (*ObservationRecall)(nil)
	_ pipeline.Conditional[*read.ReadState] = (*ObservationRecall)(nil)
)
