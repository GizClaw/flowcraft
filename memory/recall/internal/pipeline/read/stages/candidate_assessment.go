package stages

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
)

const assessmentMetadataKey = "candidate_assessment"

// CandidateAssessment centralizes read-side relevance scoring. Earlier stages
// are discovery/provenance only; this stage produces the score that rank uses.
type CandidateAssessment struct{}

func NewCandidateAssessment() *CandidateAssessment { return &CandidateAssessment{} }

func (CandidateAssessment) Name() string { return "candidate_assessment" }

func (s *CandidateAssessment) Run(_ context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	items := state.AfterTrust
	if len(items) == 0 && !state.PolicyFiltered {
		read.PromoteMergedItems(state)
		items = state.MergedItems
	}
	intent := assessmentIntent(state)
	out := make([]domain.ContextItem, 0, len(items))
	components := make([]diagnostic.CandidateAssessmentComponent, 0, len(items))
	for _, item := range items {
		component := assessCandidate(item, intent)
		components = append(components, component)
		if component.DropReason != "" {
			continue
		}
		item.Candidate.Score = component.RelevanceScore
		item.Ref.Score = component.RelevanceScore
		if item.Candidate.Metadata == nil {
			item.Candidate.Metadata = map[string]any{}
		}
		item.Candidate.Metadata[assessmentMetadataKey] = map[string]any{
			"support_score":    component.SupportScore,
			"structured_score": component.StructuredScore,
			"literal_score":    component.LiteralScore,
			"source_prior":     component.SourcePrior,
			"relevance_score":  component.RelevanceScore,
		}
		out = append(out, item)
	}
	state.AfterTrust = out
	state.AssessmentApplied = true
	detail := diagnostic.CandidateAssessmentDetail{
		InputCount:  len(items),
		OutputCount: len(out),
		Dropped:     len(items) - len(out),
		Components:  components,
	}
	if snapshotsEnabled(state) {
		snaps := contextItemSnapshots(out)
		detail.Items = candidateSnapshotPtr(snaps)
	}
	return detail, nil
}

func assessmentIntent(state *read.ReadState) domain.QueryIntent {
	var intent domain.QueryIntent
	if state != nil && state.Plan != nil {
		intent = state.Plan.Intent
	} else if state != nil && state.Intent != nil {
		intent = *state.Intent
	} else if state != nil {
		intent = domain.QueryIntent{Text: state.Query.Text, Entities: state.Query.Entities, Subject: state.Query.Subject, Predicate: state.Query.Predicate, Object: state.Query.Object}
	}
	if strings.TrimSpace(intent.Text) != "" {
		intent.Features = recallintent.ExtractFeatures(intent.Text)
	}
	return intent
}

func assessCandidate(item domain.ContextItem, intent domain.QueryIntent) diagnostic.CandidateAssessmentComponent {
	component := diagnostic.CandidateAssessmentComponent{
		ID:   item.Candidate.ID,
		Kind: string(item.Candidate.Kind),
	}
	component.SupportScore = assessmentSupportScore(item)
	component.StructuredScore = assessmentStructuredScore(item, intent)
	component.LiteralScore = assessmentLiteralScore(item, intent)
	component.SourcePrior = assessmentSourcePrior(item)
	score := component.SupportScore + component.StructuredScore + component.LiteralScore + component.SourcePrior
	if item.Candidate.Score > 0 {
		score += math.Min(item.Candidate.Score, 1.0) * 0.10
	}
	if score > 1 {
		score = 1
	}
	component.RelevanceScore = score
	if component.SupportScore == 0 {
		component.DropReason = "unsupported_candidate"
	} else if assessmentIntentHasAnchor(intent) && component.StructuredScore == 0 && component.LiteralScore == 0 && !assessmentAllowsLinkedSupport(item) {
		component.DropReason = "no_query_anchor_match"
	}
	return component
}

func assessmentAllowsLinkedSupport(item domain.ContextItem) bool {
	return item.Candidate.Source == linkExpansionSource ||
		item.Ref.Source == linkExpansionSource ||
		metadataHasSource(item.Candidate.Metadata, linkExpansionSource) ||
		metadataHasSource(item.Ref.Metadata, linkExpansionSource)
}

func metadataHasSource(md map[string]any, source string) bool {
	for _, existing := range metadataSources(md) {
		if existing == source {
			return true
		}
	}
	return false
}

func assessmentIntentHasAnchor(intent domain.QueryIntent) bool {
	return strings.TrimSpace(intent.Subject) != "" ||
		strings.TrimSpace(intent.Predicate) != "" ||
		strings.TrimSpace(intent.Object) != "" ||
		len(intent.Entities) > 0 ||
		len(intent.Kinds) > 0 ||
		len(intent.Features.Proper) > 0 ||
		len(intent.Features.Numeric) > 0 ||
		len(intent.Features.Quoted) > 0
}

func assessmentSupportScore(item domain.ContextItem) float64 {
	score := 0.0
	if item.Fact.ID != "" || item.Observation.ID != "" || item.Link.ID != "" {
		score += 0.35
	}
	if len(item.Evidence) > 0 || len(item.Fact.EvidenceRefs) > 0 {
		score += 0.20
	}
	if item.Observation.ID != "" && len(item.Observation.Spans) > 0 {
		score += 0.10
	}
	if score > 0.55 {
		return 0.55
	}
	return score
}

func assessmentStructuredScore(item domain.ContextItem, intent domain.QueryIntent) float64 {
	score := 0.0
	if intent.Subject != "" && fieldTokenOverlap(intent.Subject, item.Fact.Subject, item.Fact.Content) {
		score += 0.12
	}
	if intent.Predicate != "" && fieldTokenOverlap(intent.Predicate, item.Fact.Predicate, item.Fact.Content) {
		score += 0.08
	}
	if intent.Object != "" && fieldTokenOverlap(intent.Object, item.Fact.Object, item.Fact.Content) {
		score += 0.10
	}
	if entityOverlap(intent.Entities, item.Fact.Entities, item.Fact.Participants, []string{item.Fact.Subject, item.Fact.Object}) {
		score += 0.12
	}
	if factKindMatches(intent.Kinds, item.Fact.Kind) {
		score += 0.08
	}
	if factWithinTimeRange(intent.TimeRange, item.Fact.ObservedAt, item.Observation.ObservedAt) {
		score += 0.08
	}
	if score > 0.30 {
		return 0.30
	}
	return score
}

func factKindMatches(kinds []domain.FactKind, kind domain.FactKind) bool {
	for _, candidate := range kinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

func factWithinTimeRange(r domain.TimeRange, times ...time.Time) bool {
	if r.IsZero() {
		return false
	}
	for _, t := range times {
		if t.IsZero() {
			continue
		}
		if !r.From.IsZero() && t.Before(r.From) {
			continue
		}
		if !r.To.IsZero() && t.After(r.To) {
			continue
		}
		return true
	}
	return false
}

func assessmentLiteralScore(item domain.ContextItem, intent domain.QueryIntent) float64 {
	textTokens := recallintent.TextTokenSet(assessmentText(item))
	score := 0.0
	if tokenSetIntersects(recallintent.TextTokenSet(intent.Text), textTokens) {
		score += 0.08
	}
	if tokenSetIntersects(intent.Features.Proper, textTokens) {
		score += 0.08
	}
	if tokenSetIntersects(intent.Features.Numeric, textTokens) {
		score += 0.08
	}
	if tokenSetIntersects(intent.Features.Quoted, textTokens) {
		score += 0.08
	}
	if intent.Features.HasTimeSignal() && item.Fact.ObservedAt.IsZero() && item.Observation.ObservedAt.IsZero() {
		score -= 0.04
	}
	if score < 0 {
		return 0
	}
	if score > 0.20 {
		return 0.20
	}
	return score
}

func assessmentSourcePrior(item domain.ContextItem) float64 {
	switch item.Candidate.Kind {
	case domain.GraphNodeAssertion:
		return 0.05
	case domain.GraphNodeObservation:
		return 0.03
	default:
		return 0.02
	}
}

func fieldTokenOverlap(query string, fields ...string) bool {
	queryTokens := recallintent.TextTokenSet(query)
	if len(queryTokens) == 0 {
		return false
	}
	for _, field := range fields {
		if tokenSetIntersects(queryTokens, recallintent.TextTokenSet(field)) {
			return true
		}
	}
	return false
}

func entityOverlap(query []string, groups ...[]string) bool {
	queryTokens := map[string]struct{}{}
	for _, entity := range query {
		for token := range recallintent.TextTokenSet(entity) {
			queryTokens[token] = struct{}{}
		}
	}
	if len(queryTokens) == 0 {
		return false
	}
	for _, group := range groups {
		for _, value := range group {
			if tokenSetIntersects(queryTokens, recallintent.TextTokenSet(value)) {
				return true
			}
		}
	}
	return false
}

func assessmentText(item domain.ContextItem) string {
	var parts []string
	parts = append(parts, item.Fact.Content, item.Fact.Subject, item.Fact.Predicate, item.Fact.Object, item.Fact.EvidenceText)
	parts = append(parts, item.Fact.Entities...)
	parts = append(parts, item.Fact.Participants...)
	if item.Observation.Text != "" {
		parts = append(parts, item.Observation.Text)
	}
	for _, span := range item.Observation.Spans {
		parts = append(parts, span.Text)
	}
	for _, ref := range item.Evidence {
		parts = append(parts, ref.Text)
	}
	return strings.Join(parts, " ")
}

var _ pipeline.Stage[*read.ReadState] = (*CandidateAssessment)(nil)
