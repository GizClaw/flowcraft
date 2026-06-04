package stages

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/words"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

type deterministicCandidateAssessor struct {
	semantic port.SemanticScorer
}

func (a deterministicCandidateAssessor) Assess(ctx context.Context, in domain.AssessmentInput) (domain.CandidateAssessment, error) {
	item := in.Item
	intent := in.Intent
	if strings.TrimSpace(intent.Text) == "" {
		intent.Text = in.QueryText
	}
	if strings.TrimSpace(intent.Text) != "" && intent.Features.IsZero() {
		intent.Features = recallintent.ExtractFeatures(intent.Text)
	}
	links := append(assessmentItemLinks(item), in.Links...)
	structured := assessmentStructuredSignals(item, intent, links)
	literal := assessmentLiteralSignals(item, intent)
	surface := assessmentExactSurfaceSignals(item, intent)
	literalCompatible := assessmentLiteralCompatible(item, intent, structured, links)
	assessment := domain.CandidateAssessment{
		HardConstraintPass: true,
		SupportScore:       assessmentSupportScore(item, links),
		StructuredScore:    math.Min(0.30, structured.Score+surface.Score),
		LiteralScore:       literal.Score,
		EquivalenceGroup:   assessmentEquivalenceGroup(item),
		SupportGroup:       assessmentSupportGroup(item),
		DiversityGroup:     assessmentDiversityGroup(item),
	}
	if assessment.SupportScore == 0 && assessmentHasAuditableExactAnchor(item, structured, literal, surface, literalCompatible) {
		assessment.SupportScore = 0.20
	}
	if a.semantic == nil {
		assessment.FallbackReason = "semantic_scorer_unavailable"
	} else if strings.TrimSpace(in.QueryText) != "" || strings.TrimSpace(intent.Text) != "" {
		score, reason, err := a.semantic.Score(ctx, firstNonEmpty(in.QueryText, intent.Text), item)
		if err != nil {
			assessment.FallbackReason = "semantic_scorer_error"
		} else if score > 0 {
			assessment.SemanticScore = math.Min(score, 1)
			if reason != "" {
				assessment.Reason = reason
			}
		}
	}
	score := assessment.SupportScore + assessment.StructuredScore + assessment.LiteralScore + assessment.SemanticScore
	if score > 1 {
		score = 1
	}
	assessment.RelevanceScore = score
	hasAnchor := assessmentIntentHasAnchor(intent, nil)
	deterministicExactPass := assessmentDeterministicExactPass(item, intent, structured, literal, surface, literalCompatible, assessment)
	switch {
	case assessment.SupportScore == 0:
		assessment.DropReason = "unsupported_candidate"
	case hasAnchor && structured.HasMismatch && assessment.SemanticScore == 0:
		assessment.DropReason = "no_query_anchor_match"
	case assessment.SemanticScore == 0 && assessment.FallbackReason == "semantic_scorer_unavailable" && !deterministicExactPass:
		if hasAnchor {
			assessment.DropReason = "no_query_anchor_match"
		} else {
			assessment.DropReason = "semantic_scorer_unavailable"
		}
	case hasAnchor && !deterministicExactPass && structured.QueryAnchorScore == 0 && (!literal.Pass || !literalCompatible) && !surface.Pass && assessment.SemanticScore == 0:
		assessment.DropReason = "no_query_anchor_match"
	case !hasAnchor && !deterministicExactPass && assessment.SemanticScore == 0:
		assessment.DropReason = "semantic_scorer_unavailable"
	}
	if assessment.DropReason != "" {
		assessment.RelevanceScore = 0
	}
	assessment.Confidence = assessmentConfidence(assessmentComponent(item, assessment))
	if assessment.Reason == "" {
		assessment.Reason = assessmentReason(assessmentComponent(item, assessment))
	}
	return assessment, nil
}

func assessmentDeterministicExactPass(item domain.ContextItem, intent domain.QueryIntent, structured structuredAssessmentSignals, literal literalAssessmentSignals, surface surfaceAssessmentSignals, literalCompatible bool, assessment domain.CandidateAssessment) bool {
	if assessment.SemanticScore > 0 {
		return true
	}
	if structured.HasMismatch {
		return false
	}
	if assessmentQuestionExactRequiresCompatibility(intent) {
		if assessmentExplicitTypedAnchorCompatible(item, intent) {
			return true
		}
		if !assessmentItemIsAuditableRenderable(item) || (!surface.Pass && !literal.Pass) {
			return false
		}
		return assessmentQuestionAnswerShapeCompatible(intent, item.Fact)
	}
	if surface.Pass && assessmentItemIsAuditableRenderable(item) {
		return true
	}
	if structured.QueryAnchorScore > 0 {
		return true
	}
	return literal.Pass && literalCompatible
}

func assessmentHasAuditableExactAnchor(item domain.ContextItem, structured structuredAssessmentSignals, literal literalAssessmentSignals, surface surfaceAssessmentSignals, literalCompatible bool) bool {
	return assessmentItemIsAuditableRenderable(item) &&
		(surface.Pass || (literal.Pass && literalCompatible) || structured.QueryAnchorScore > 0)
}

func assessmentItemIsAuditableRenderable(item domain.ContextItem) bool {
	if item.Fact.ID != "" && canonicalFactHasRenderableClaim(item.Fact) {
		return true
	}
	return item.Observation.ID != "" && strings.TrimSpace(item.Observation.Text) != ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func assessmentIntentHasAnchor(intent domain.QueryIntent, state *read.ReadState) bool {
	if state != nil {
		if len(state.Query.Entities) > 0 ||
			len(state.Query.Kinds) > 0 ||
			strings.TrimSpace(state.Query.Subject) != "" ||
			strings.TrimSpace(state.Query.Predicate) != "" ||
			strings.TrimSpace(state.Query.Object) != "" ||
			!state.Query.TimeRange.IsZero() {
			return true
		}
	}
	return strings.TrimSpace(intent.Subject) != "" ||
		strings.TrimSpace(intent.Predicate) != "" ||
		strings.TrimSpace(intent.Object) != "" ||
		len(intent.Entities) > 0 ||
		len(intent.Kinds) > 0 ||
		!intent.TimeRange.IsZero() ||
		len(intent.Features.Numeric) > 0 ||
		len(intent.Features.Quoted) > 0 ||
		assessmentHasExactTimeAnchor(intent.Features)
}

func assessmentHasExactTimeAnchor(features domain.QueryFeatures) bool {
	return features.Temporal.HasExplicitDate || !features.Temporal.TimeRange.IsZero()
}

func assessmentSupportScore(item domain.ContextItem, links []domain.FactLink) float64 {
	score := 0.0
	if item.Observation.ID != "" {
		score += 0.45
		if len(item.Observation.Spans) > 0 {
			score += 0.10
		}
	}
	if len(item.Evidence) > 0 || len(item.Fact.EvidenceRefs) > 0 {
		score += 0.35
	}
	if item.Fact.ID != "" && item.Fact.Kind != "" && canonicalFactHasRenderableClaim(item.Fact) {
		score += 0.20
	}
	for _, link := range dedupeAssessmentLinks(links) {
		switch link.Type {
		case domain.LinkSupports:
			score += 0.35
		case domain.LinkAnswersSlot, domain.LinkResolvesTo:
			score += 0.30
		case domain.LinkDerivedFrom:
			score += 0.25
		case domain.LinkSameEventAs:
			score += 0.12
		case domain.LinkSameObservation:
			score += 0.05
		}
	}
	if score > 0.65 {
		return 0.65
	}
	return score
}

func canonicalFactHasRenderableClaim(f domain.TemporalFact) bool {
	return strings.TrimSpace(f.Content) != "" ||
		strings.TrimSpace(f.Subject) != "" ||
		strings.TrimSpace(f.Predicate) != "" ||
		strings.TrimSpace(f.Object) != ""
}

type structuredAssessmentSignals struct {
	Score            float64
	QueryAnchorScore float64
	HasMismatch      bool
}

func assessmentStructuredSignals(item domain.ContextItem, intent domain.QueryIntent, links []domain.FactLink) structuredAssessmentSignals {
	var out structuredAssessmentSignals
	if exactStringMatch(intent.Subject, item.Fact.Subject) {
		out.Score += 0.08
		out.QueryAnchorScore += 0.08
	} else if strings.TrimSpace(intent.Subject) != "" && strings.TrimSpace(item.Fact.Subject) != "" {
		out.HasMismatch = true
	}
	if exactStringMatch(intent.Predicate, item.Fact.Predicate) {
		out.Score += 0.06
		out.QueryAnchorScore += 0.06
	} else if strings.TrimSpace(intent.Predicate) != "" && strings.TrimSpace(item.Fact.Predicate) != "" {
		out.HasMismatch = true
	}
	if exactStringMatch(intent.Object, item.Fact.Object) {
		out.Score += 0.08
		out.QueryAnchorScore += 0.08
	} else if strings.TrimSpace(intent.Object) != "" && strings.TrimSpace(item.Fact.Object) != "" {
		out.HasMismatch = true
	}
	if exactEntityMatch(intent.Entities, item.Fact.Entities, item.Fact.Participants, []string{item.Fact.Subject, item.Fact.Object}) {
		out.Score += 0.10
		out.QueryAnchorScore += 0.10
	}
	if factKindMatches(intent.Kinds, item.Fact.Kind) {
		out.Score += 0.06
		out.QueryAnchorScore += 0.06
	}
	if factWithinTimeRange(assessmentIntentTimeRange(intent), item.Fact.ObservedAt, item.Observation.ObservedAt) {
		out.Score += 0.06
		out.QueryAnchorScore += 0.06
	}
	for _, link := range dedupeAssessmentLinks(links) {
		switch link.Type {
		case domain.LinkAnswersSlot, domain.LinkResolvesTo:
			out.Score += 0.18
		case domain.LinkSameEventAs:
			out.Score += 0.06
		case domain.LinkDerivedFrom:
			out.Score += 0.02
		case domain.LinkSameObservation:
			out.Score += 0.02
		}
	}
	if out.Score > 0.30 {
		out.Score = 0.30
	}
	if out.QueryAnchorScore > out.Score {
		out.QueryAnchorScore = out.Score
	}
	return out
}

func assessmentQuestionExactRequiresCompatibility(intent domain.QueryIntent) bool {
	return assessmentQuestionAnswerShape(intent.Text) != ""
}

func assessmentQuestionAnswerShape(text string) string {
	for _, token := range tokenize.SplitWords(text) {
		token = strings.ToLower(strings.TrimSpace(token))
		if shape := assessmentQuestionShapeToken(token); shape != "" {
			return shape
		}
		if token == "" || assessmentQuestionShapePrefixToken(token) {
			continue
		}
		return ""
	}
	return ""
}

func assessmentQuestionShapeToken(token string) string {
	switch token {
	case "where", "who", "whom", "whose", "when", "what", "which", "how":
		return token
	default:
		return ""
	}
}

func assessmentQuestionShapePrefixToken(token string) bool {
	switch token {
	case "do", "does", "did",
		"can", "could", "would", "will", "should", "shall", "may", "might",
		"please", "pls",
		"you", "u", "me", "us",
		"know", "tell", "happen", "happened", "to",
		"recall", "remember", "remind", "show", "find", "give":
		return true
	default:
		return false
	}
}

func assessmentExplicitTypedAnchorCompatible(item domain.ContextItem, intent domain.QueryIntent) bool {
	fact := item.Fact
	return exactStringMatch(intent.Subject, fact.Subject) ||
		exactStringMatch(intent.Predicate, fact.Predicate) ||
		exactStringMatch(intent.Object, fact.Object) ||
		factKindMatches(intent.Kinds, fact.Kind) ||
		factWithinTimeRange(assessmentIntentTimeRange(intent), fact.ObservedAt, item.Observation.ObservedAt)
}

func assessmentQuestionAnswerShapeCompatible(intent domain.QueryIntent, fact domain.TemporalFact) bool {
	predicateTokens := assessmentSignificantSurfaceTokens(fact.Predicate)
	switch assessmentQuestionAnswerShape(intent.Text) {
	case "where":
		return assessmentPredicateHasAny(predicateTokens, "location", "located", "located at", "city", "address", "place", "venue", "room")
	case "when":
		return assessmentPredicateHasAny(predicateTokens, "time", "date", "deadline", "due", "scheduled", "scheduled at")
	case "who", "whom", "whose":
		return assessmentPredicateHasAny(predicateTokens, "person", "owner", "owned by", "author", "speaker", "participant", "assignee", "contact")
	default:
		return false
	}
}

func assessmentPredicateHasAny(predicateTokens []string, phrases ...string) bool {
	if len(predicateTokens) == 0 {
		return false
	}
	tokenSet := make(map[string]struct{}, len(predicateTokens))
	for _, token := range predicateTokens {
		tokenSet[token] = struct{}{}
	}
	for _, phrase := range phrases {
		if assessmentTokensCovered(assessmentSignificantSurfaceTokens(phrase), tokenSet) {
			return true
		}
	}
	return false
}

func assessmentIntentTimeRange(intent domain.QueryIntent) domain.TimeRange {
	if !intent.TimeRange.IsZero() {
		return intent.TimeRange
	}
	return intent.Features.Temporal.TimeRange
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

type literalAssessmentSignals struct {
	Score float64
	Pass  bool
}

func assessmentLiteralSignals(item domain.ContextItem, intent domain.QueryIntent) literalAssessmentSignals {
	textTokens := lowerTokenSet(recallintent.TextTokenSet(assessmentText(item)))
	var out literalAssessmentSignals
	quotedMatches, quotedTotal := exactTokenMatchCount(lowerTokenSet(intent.Features.Quoted), textTokens)
	if quotedTotal > 0 && quotedMatches == quotedTotal {
		if quotedTotal >= 2 {
			out.Score += 0.08
			out.Pass = true
		} else {
			out.Score += 0.03
		}
	}
	numericMatches, numericTotal := exactTokenMatchCount(lowerTokenSet(intent.Features.Numeric), textTokens)
	if numericTotal > 0 && numericMatches == numericTotal {
		if numericTotal >= 2 {
			out.Score += 0.08
			out.Pass = true
		} else {
			out.Score += 0.02
		}
	}
	if assessmentHasExactTimeAnchor(intent.Features) && item.Fact.ObservedAt.IsZero() && item.Observation.ObservedAt.IsZero() {
		out.Score -= 0.04
	}
	if factWithinTimeRange(assessmentIntentTimeRange(intent), item.Fact.ObservedAt, item.Observation.ObservedAt) {
		out.Score += 0.04
		out.Pass = true
	}
	if out.Score < 0 {
		out.Score = 0
	}
	if out.Score > 0.20 {
		out.Score = 0.20
	}
	return out
}

type surfaceAssessmentSignals struct {
	Score float64
	Pass  bool
}

func assessmentExactSurfaceSignals(item domain.ContextItem, intent domain.QueryIntent) surfaceAssessmentSignals {
	surfaces := assessmentSurfaceTexts(item)
	if len(surfaces) == 0 {
		return surfaceAssessmentSignals{}
	}
	surfaceTokens := assessmentSurfaceWordSet(strings.Join(surfaces, " "))
	if len(surfaceTokens) == 0 {
		return surfaceAssessmentSignals{}
	}
	var out surfaceAssessmentSignals
	if tokenSetIntersects(lowerTokenSet(intent.Features.Proper), surfaceTokens) {
		out.Score += 0.08
		out.Pass = true
	}
	if out.Score > 0.12 {
		out.Score = 0.12
	}
	return out
}

func assessmentLiteralCompatible(item domain.ContextItem, intent domain.QueryIntent, structured structuredAssessmentSignals, links []domain.FactLink) bool {
	if structured.QueryAnchorScore > 0 && !structured.HasMismatch {
		return true
	}
	if strings.TrimSpace(intent.Predicate) != "" {
		return exactStringMatch(intent.Predicate, item.Fact.Predicate)
	}
	for _, link := range dedupeAssessmentLinks(links) {
		switch link.Type {
		case domain.LinkAnswersSlot, domain.LinkResolvesTo:
			return true
		}
	}
	return false
}

func assessmentSurfaceTexts(item domain.ContextItem) []string {
	return []string{assessmentText(item)}
}

func assessmentSignificantSurfaceTokens(text string) []string {
	raw := tokenize.SplitWords(text)
	if len(raw) == 0 {
		return nil
	}
	out := make([]string, 0, len(raw))
	seen := map[string]struct{}{}
	for _, token := range raw {
		token = strings.ToLower(strings.TrimSpace(token))
		if token == "" || assessmentSurfaceQueryStopword(token) {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}

func assessmentSurfaceWordSet(text string) map[string]struct{} {
	raw := tokenize.SplitWords(text)
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(raw))
	for _, token := range raw {
		token = strings.ToLower(strings.TrimSpace(token))
		if token != "" {
			out[token] = struct{}{}
		}
	}
	return out
}

func assessmentSurfaceQueryStopword(token string) bool {
	switch token {
	case "now", "currently", "next", "previous", "earlier", "later":
		return true
	default:
		return words.IsIntentEntityStopword(token)
	}
}

func assessmentTokensCovered(want []string, have map[string]struct{}) bool {
	if len(want) == 0 || len(have) == 0 {
		return false
	}
	for _, token := range want {
		if _, ok := have[token]; !ok {
			return false
		}
	}
	return true
}

func exactTokenMatchCount(want, have map[string]struct{}) (int, int) {
	if len(want) == 0 || len(have) == 0 {
		return 0, len(want)
	}
	matches := 0
	for token := range want {
		if _, ok := have[token]; ok {
			matches++
		}
	}
	return matches, len(want)
}

func dedupeAssessmentLinks(links []domain.FactLink) []domain.FactLink {
	if len(links) == 0 {
		return nil
	}
	out := make([]domain.FactLink, 0, len(links))
	seen := map[string]struct{}{}
	for _, link := range links {
		if link.Type == "" {
			continue
		}
		key := link.ID
		if key == "" {
			key = string(link.Type) + ":" + string(link.From.Kind) + ":" + link.From.ID + ":" + string(link.To.Kind) + ":" + link.To.ID
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, link)
	}
	return out
}

func lowerTokenSet(tokens map[string]struct{}) map[string]struct{} {
	if len(tokens) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(tokens))
	for token := range tokens {
		token = strings.ToLower(strings.TrimSpace(token))
		if token != "" {
			out[token] = struct{}{}
		}
	}
	return out
}

func exactStringMatch(a, b string) bool {
	a = normalizedExactValue(a)
	b = normalizedExactValue(b)
	return a != "" && b != "" && a == b
}

func exactEntityMatch(query []string, groups ...[]string) bool {
	entities := map[string]struct{}{}
	for _, entity := range query {
		if entity = normalizedExactValue(entity); entity != "" {
			entities[entity] = struct{}{}
		}
	}
	if len(entities) == 0 {
		return false
	}
	for _, group := range groups {
		for _, value := range group {
			if _, ok := entities[normalizedExactValue(value)]; ok {
				return true
			}
		}
	}
	return false
}

func normalizedExactValue(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func assessmentEquivalenceGroup(item domain.ContextItem) string {
	if item.Fact.ID != "" {
		return "fact:" + item.Fact.ID
	}
	if item.Observation.ID != "" {
		return "obs:" + item.Observation.ID
	}
	if item.Link.ID != "" {
		return "link:" + item.Link.ID
	}
	return ""
}

func assessmentSupportGroup(item domain.ContextItem) string {
	if item.Link.MergeKey != "" {
		return "link:" + item.Link.MergeKey
	}
	for _, ref := range append(append([]domain.EvidenceRef(nil), item.Evidence...), item.Fact.EvidenceRefs...) {
		if ref.ObservationID != "" {
			return "obs:" + ref.ObservationID
		}
		if ref.MessageID != "" {
			return "msg:" + ref.MessageID
		}
		if ref.ID != "" {
			return "ev:" + ref.ID
		}
	}
	if item.Observation.ID != "" {
		return "obs:" + item.Observation.ID
	}
	return ""
}

func assessmentDiversityGroup(item domain.ContextItem) string {
	if item.Fact.Kind != "" {
		return "kind:" + string(item.Fact.Kind)
	}
	if item.Candidate.Source != "" {
		return "source:" + item.Candidate.Source
	}
	return ""
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
