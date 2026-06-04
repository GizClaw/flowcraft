package stages

import (
	"context"
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
	assessmentMetadataKey = "candidate_assessment"
)

// CandidateAssessment centralizes read-side relevance scoring. Earlier stages
// are discovery/provenance only; this stage produces the score that rank uses.
type CandidateAssessment struct {
	assessor      port.CandidateAssessor
	semantic      port.SemanticScorer
	supportReader port.SupportReader
}

type CandidateAssessmentOption func(*CandidateAssessment)

func WithAssessmentAssessor(assessor port.CandidateAssessor) CandidateAssessmentOption {
	return func(s *CandidateAssessment) {
		if assessor != nil {
			s.assessor = assessor
		}
	}
}

func WithAssessmentSemanticScorer(scorer port.SemanticScorer) CandidateAssessmentOption {
	return func(s *CandidateAssessment) {
		if scorer != nil {
			s.semantic = scorer
		}
	}
}

func WithAssessmentSupportReader(reader port.SupportReader) CandidateAssessmentOption {
	return func(s *CandidateAssessment) {
		if reader != nil {
			s.supportReader = reader
		}
	}
}

func NewCandidateAssessment(opts ...CandidateAssessmentOption) *CandidateAssessment {
	stage := &CandidateAssessment{}
	for _, opt := range opts {
		if opt != nil {
			opt(stage)
		}
	}
	if stage.assessor == nil {
		stage.assessor = deterministicCandidateAssessor{semantic: stage.semantic}
	}
	return stage
}

func (CandidateAssessment) Name() string { return "candidate_assessment" }

func (s *CandidateAssessment) Run(ctx context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	items := state.AfterTrust
	if len(items) == 0 && !state.PolicyFiltered {
		read.PromoteMergedItems(state)
		items = state.MergedItems
	}
	intent := assessmentIntent(state)
	out := make([]domain.ContextItem, 0, len(items))
	components := make([]diagnostic.CandidateAssessmentComponent, 0, len(items))
	for _, item := range items {
		in, err := s.assessmentInput(ctx, item, intent, state)
		if err != nil {
			return diagnostic.CandidateAssessmentDetail{}, err
		}
		assessment, err := s.assessor.Assess(ctx, in)
		if err != nil {
			return diagnostic.CandidateAssessmentDetail{}, err
		}
		component := assessmentComponent(item, assessment)
		components = append(components, component)
		state.RecordCandidateAssessment(item, assessment)
		if component.DropReason != "" {
			continue
		}
		if item.Candidate.Metadata == nil {
			item.Candidate.Metadata = map[string]any{}
		}
		item.Candidate.Metadata[assessmentMetadataKey] = map[string]any{
			"support_score":    component.SupportScore,
			"structured_score": component.StructuredScore,
			"literal_score":    component.LiteralScore,
			"semantic_score":   component.SemanticScore,
			"source_prior":     component.SourcePrior,
			"relevance_score":  component.RelevanceScore,
			"confidence":       component.Confidence,
			"reason":           component.Reason,
			"drop_reason":      component.DropReason,
			"fallback_reason":  component.FallbackReason,
			"support_group":    component.SupportGroup,
			"diversity_group":  component.DiversityGroup,
		}
		out = append(out, item)
	}
	state.AssessedItems = out
	state.AssessmentApplied = true
	detail := diagnostic.CandidateAssessmentDetail{
		InputCount:   len(items),
		Accepted:     len(out),
		Rejected:     len(items) - len(out),
		OutputCount:  len(out),
		Dropped:      len(items) - len(out),
		DropReasons:  assessmentDropReasons(components),
		ScoreSummary: assessmentScoreSummary(components),
		Components:   components,
	}
	if snapshotsEnabled(state) {
		inputSnaps := contextItemSnapshotsWithStateScoreLabel(state, items, scoreLabelDiscovery)
		acceptedSnaps := contextItemSnapshotsWithStateScoreLabel(state, out, scoreLabelAssessment)
		rejectedSnaps := assessmentRejectedSnapshots(items, components)
		detail.Input = candidateSnapshotPtr(inputSnaps)
		detail.AcceptedItems = candidateSnapshotPtr(acceptedSnaps)
		detail.RejectedItems = candidateSnapshotPtr(rejectedSnaps)
		detail.Items = candidateSnapshotPtr(acceptedSnaps)
	}
	return detail, nil
}

func (s *CandidateAssessment) assessmentInput(ctx context.Context, item domain.ContextItem, intent domain.QueryIntent, state *read.ReadState) (domain.AssessmentInput, error) {
	links := assessmentItemLinks(item)
	if s != nil && s.supportReader != nil {
		readLinks, err := s.supportReader.LinksForCandidate(ctx, assessmentScope(item, state), item)
		if err != nil {
			return domain.AssessmentInput{}, err
		}
		links = append(links, readLinks...)
	}
	queryText := intent.Text
	if strings.TrimSpace(queryText) == "" && state != nil {
		queryText = state.Query.Text
	}
	now := time.Time{}
	if state != nil {
		now = state.Now
	}
	return domain.AssessmentInput{
		QueryText: queryText,
		Intent:    intent,
		Item:      item,
		Evidence:  assessmentEvidence(item),
		Links:     links,
		Signals:   domain.ContextItemDiscoverySignals(item),
		Now:       now,
	}, nil
}

func assessmentScope(item domain.ContextItem, state *read.ReadState) domain.Scope {
	if item.Fact.Scope.RuntimeID != "" {
		return item.Fact.Scope
	}
	if item.Observation.Scope.RuntimeID != "" {
		return item.Observation.Scope
	}
	if item.Link.Scope.RuntimeID != "" {
		return item.Link.Scope
	}
	if state != nil {
		return state.Scope
	}
	return domain.Scope{}
}

func assessmentEvidence(item domain.ContextItem) []domain.EvidenceRef {
	out := make([]domain.EvidenceRef, 0, len(item.Evidence)+len(item.Fact.EvidenceRefs)+len(item.Link.EvidenceRefs))
	out = append(out, item.Evidence...)
	out = append(out, item.Fact.EvidenceRefs...)
	out = append(out, item.Link.EvidenceRefs...)
	return out
}

func assessmentItemLinks(item domain.ContextItem) []domain.FactLink {
	if item.Link.Type == "" && item.Link.ID == "" {
		return nil
	}
	return []domain.FactLink{item.Link}
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
	if state != nil {
		if strings.TrimSpace(intent.Text) == "" {
			intent.Text = state.Query.Text
		}
		if len(intent.Entities) == 0 {
			intent.Entities = append([]string(nil), state.Query.Entities...)
		}
		if strings.TrimSpace(intent.Subject) == "" {
			intent.Subject = state.Query.Subject
		}
		if strings.TrimSpace(intent.Predicate) == "" {
			intent.Predicate = state.Query.Predicate
		}
		if strings.TrimSpace(intent.Object) == "" {
			intent.Object = state.Query.Object
		}
		if len(intent.Kinds) == 0 {
			intent.Kinds = append([]domain.FactKind(nil), state.Query.Kinds...)
		}
		if intent.TimeRange.IsZero() {
			intent.TimeRange = state.Query.TimeRange
		}
	}
	if strings.TrimSpace(intent.Text) != "" {
		intent.Features = recallintent.ExtractFeatures(intent.Text)
	}
	return intent
}

var _ pipeline.Stage[*read.ReadState] = (*CandidateAssessment)(nil)
