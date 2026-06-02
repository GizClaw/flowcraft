package stages

import (
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
)

const (
	contextPackDuplicateJaccardCutoff   = 0.86
	contextPackSiblingFactJaccardFloor  = 0.10
	contextPackSiblingFactJaccardCutoff = 0.62
	contextPackSameEvidenceSiblingCap   = 2
	contextPackBaseScoreWeight          = 0.85
	contextPackRankPriorWeight          = 0.15

	contextPackSourcePenaltyPerExtra = 0.03
	contextPackSourcePenaltyMax      = 0.12
	contextPackDiversityPenaltyScale = 0.22
	contextPackDiversityPenaltyFloor = 0.35

	contextPackObservationDefaultCap = 1
	contextPackObservationRescueCap  = 4

	contextPackRankOutputAnchorDivisor = 3
)

type contextPackInput struct {
	cap int
}

type contextPackCandidate struct {
	hit            domain.Hit
	score          float64
	baseScore      float64
	queryRank      int
	evidenceKey    string
	evidenceGroup  string
	evidenceTokens map[string]struct{}
	factTokens     map[string]struct{}
}

func packRecallContextWithFeaturesAndDetail(ordered []domain.Hit, pool []domain.Hit, cap int) []domain.Hit {
	hits, _ := packRecallContextWithTrace(ordered, pool, cap)
	return hits
}

func packRecallContextWithTrace(ordered []domain.Hit, pool []domain.Hit, cap int) ([]domain.Hit, []diagnostic.CandidateSnapshot) {
	return packRecallContextWithIntentTrace(domain.QueryIntent{}, ordered, pool, cap)
}

func packRecallContextWithIntentTrace(intent domain.QueryIntent, ordered []domain.Hit, pool []domain.Hit, cap int) ([]domain.Hit, []diagnostic.CandidateSnapshot) {
	input := newContextPackInput(cap)
	if input.cap <= 0 {
		return ordered, nil
	}
	candidatePool := mergeContextPackPool(ordered, pool)
	if len(candidatePool) == 0 {
		return ordered, nil
	}
	candidates := contextPackCandidates(input, candidatePool)
	traceCandidates := append([]contextPackCandidate(nil), candidates...)
	dropReasons := map[string]string{}
	candidates = contextPackLimitObservationCandidates(candidates, dropReasons)
	candidates = contextPackLimitRouteLaneCandidates(intent, candidates, input.cap, dropReasons)
	selectedCandidates := candidates
	if len(candidates) <= input.cap {
		hits := contextPackHits(selectedCandidates)
		return hits, contextPackTrace(traceCandidates, selectedCandidates, len(ordered), dropReasons)
	}
	anchors := contextPackRankOutputAnchors(candidates, len(ordered), input.cap)
	anchors = contextPackFilterAnchorsByRoute(intent, anchors)
	selectedCandidates = contextPackFillWithMMR(candidates, anchors, input.cap)
	return contextPackHits(selectedCandidates), contextPackTrace(traceCandidates, selectedCandidates, len(ordered), dropReasons)
}

func contextPackFillWithMMR(candidates, selected []contextPackCandidate, cap int) []contextPackCandidate {
	if cap <= 0 || len(selected) >= cap {
		return selected[:min(len(selected), cap)]
	}
	out := append([]contextPackCandidate(nil), selected...)
	used := make([]bool, len(candidates))
	for i, cand := range candidates {
		if contextPackCandidateDuplicate(cand, out) {
			used[i] = true
		}
	}
	for len(out) < cap {
		best := -1
		bestScore := math.Inf(-1)
		for i, cand := range candidates {
			if used[i] || contextPackCandidateDuplicate(cand, out) {
				continue
			}
			sourcePenalty := contextPackSourceConcentrationPenalty(cand, out)
			adjusted := cand.score -
				contextPackDiversityPenalty(cand, out) -
				sourcePenalty
			if best < 0 || adjusted > bestScore || (math.Abs(adjusted-bestScore) <= 1e-9 && betterContextPackTieBreak(cand, candidates[best])) {
				best = i
				bestScore = adjusted
			}
		}
		if best < 0 {
			break
		}
		used[best] = true
		out = append(out, candidates[best])
	}
	if len(out) < cap {
		for _, cand := range candidates {
			if contextPackCandidateDuplicate(cand, out) {
				continue
			}
			out = append(out, cand)
			if len(out) >= cap {
				break
			}
		}
	}
	return out
}

func mergeContextPackPool(ordered []domain.Hit, pool []domain.Hit) []domain.Hit {
	if len(pool) == 0 {
		out := make([]domain.Hit, len(ordered))
		copy(out, ordered)
		return out
	}
	out := make([]domain.Hit, 0, len(ordered)+len(pool))
	out = append(out, ordered...)
	out = append(out, pool...)
	return out
}

func contextPackCandidates(input contextPackInput, hits []domain.Hit) []contextPackCandidate {
	maxScore := 0.0
	for _, hit := range hits {
		if hit.Score > maxScore {
			maxScore = hit.Score
		}
	}
	out := make([]contextPackCandidate, 0, len(hits))
	seenFacts := map[string]struct{}{}
	for i, hit := range hits {
		evidenceKey := primaryEvidenceKey(hit)
		candidate := newContextPackCandidate(hit, i, maxScore, evidenceKey)
		if hit.Fact.ID != "" {
			if _, ok := seenFacts[hit.Fact.ID]; ok {
				continue
			}
		}
		if contextPackCandidateDuplicate(candidate, out) {
			continue
		}
		if hit.Fact.ID != "" {
			seenFacts[hit.Fact.ID] = struct{}{}
		}
		out = append(out, candidate)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if math.Abs(out[i].score-out[j].score) > 1e-9 {
			return out[i].score > out[j].score
		}
		return betterContextPackTieBreak(out[i], out[j])
	})
	return out
}

func contextPackLimitObservationCandidates(candidates []contextPackCandidate, dropReasons ...map[string]string) []contextPackCandidate {
	if len(candidates) == 0 {
		return candidates
	}
	factCount := 0
	observationCount := 0
	for _, cand := range candidates {
		if contextPackObservationCandidate(cand) {
			observationCount++
			continue
		}
		factCount++
	}
	if observationCount == 0 {
		return candidates
	}
	cap := contextPackObservationDefaultCap
	if factCount == 0 {
		cap = contextPackObservationRescueCap
	}
	out := make([]contextPackCandidate, 0, len(candidates))
	keptObservation := 0
	for _, cand := range candidates {
		if !contextPackObservationCandidate(cand) {
			out = append(out, cand)
			continue
		}
		if keptObservation >= cap {
			contextPackRecordDropReason(dropReasons, cand, "observation_lane_cap")
			continue
		}
		out = append(out, cand)
		keptObservation++
	}
	if keptObservation == 0 && factCount == 0 {
		for _, cand := range candidates {
			if !contextPackObservationCandidate(cand) {
				continue
			}
			out = append(out, cand)
			keptObservation++
			if keptObservation >= cap {
				break
			}
		}
	}
	return out
}

func contextPackLimitRouteLaneCandidates(intent domain.QueryIntent, candidates []contextPackCandidate, cap int, dropReasons ...map[string]string) []contextPackCandidate {
	if len(candidates) == 0 || cap <= 0 {
		return candidates
	}
	directCount := 0
	for _, cand := range candidates {
		if contextPackDirectCandidate(cand.hit) || contextPackObservationCandidate(cand) {
			directCount++
		}
	}
	entityLimit := 1
	if contextPackEntityStrategy(intent) {
		entityLimit = 2
	}
	timelineLimit := 1
	if contextPackTemporalIntent(intent) {
		timelineLimit = 3
	}
	graphLimit := 1
	if contextPackBridgeStrategy(intent) && len(intent.Entities) >= 2 {
		graphLimit = 2
	}
	if directCount == 0 {
		rescueCap := min(cap, 3)
		entityLimit = max(entityLimit, rescueCap)
		timelineLimit = max(timelineLimit, rescueCap)
		graphLimit = max(graphLimit, min(cap, 2))
	}

	counts := map[string]int{}
	out := make([]contextPackCandidate, 0, len(candidates))
	for _, cand := range candidates {
		lane := contextPackRouteLane(cand.hit)
		switch lane {
		case "entity":
			if counts[lane] >= entityLimit {
				contextPackRecordDropReason(dropReasons, cand, "strategy_lane_cap")
				continue
			}
		case "timeline":
			if counts[lane] >= timelineLimit {
				contextPackRecordDropReason(dropReasons, cand, "strategy_lane_cap")
				continue
			}
		case "graph":
			if counts[lane] >= graphLimit {
				contextPackRecordDropReason(dropReasons, cand, "strategy_lane_cap")
				continue
			}
		}
		if lane != "" {
			counts[lane]++
		}
		out = append(out, cand)
	}
	return out
}

func contextPackRecordDropReason(dropReasons []map[string]string, cand contextPackCandidate, reason string) {
	if len(dropReasons) == 0 || dropReasons[0] == nil {
		return
	}
	dropReasons[0][contextPackCandidateTraceKey(cand)] = reason
}

func contextPackDirectCandidate(hit domain.Hit) bool {
	return contextPackHasRoute(hit, "retrieval") ||
		contextPackHasRoute(hit, "assertion") ||
		contextPackHasRoute(hit, "relation") ||
		contextPackHasRoute(hit, "profile")
}

func contextPackRouteLane(hit domain.Hit) string {
	if contextPackDirectCandidate(hit) {
		return ""
	}
	switch {
	case contextPackHasRoute(hit, "timeline"):
		return "timeline"
	case contextPackHasRoute(hit, "entity"):
		return "entity"
	case contextPackHasRoute(hit, "graph"):
		return "graph"
	default:
		return ""
	}
}

func contextPackEntityStrategy(intent domain.QueryIntent) bool {
	switch intent.Route.EffectiveStrategy() {
	case domain.RecallStrategySet, domain.RecallStrategyCount, domain.RecallStrategyIntersection, domain.RecallStrategyProfile:
		return true
	default:
		return false
	}
}

func contextPackTemporalIntent(intent domain.QueryIntent) bool {
	return !intent.TimeRange.IsZero() || intent.Route.EffectiveStrategy() == domain.RecallStrategyTemporal
}

func contextPackBridgeStrategy(intent domain.QueryIntent) bool {
	switch intent.Route.EffectiveStrategy() {
	case domain.RecallStrategyJoin, domain.RecallStrategyIntersection:
		return true
	default:
		return false
	}
}

func contextPackRankOutputAnchors(candidates []contextPackCandidate, rankOutputCount, cap int) []contextPackCandidate {
	anchorCap := contextPackRankOutputAnchorCount(cap)
	if anchorCap <= 0 || rankOutputCount <= 0 {
		return nil
	}
	byRank := append([]contextPackCandidate(nil), candidates...)
	sort.SliceStable(byRank, func(i, j int) bool {
		return byRank[i].queryRank < byRank[j].queryRank
	})
	out := make([]contextPackCandidate, 0, anchorCap)
	for _, cand := range byRank {
		if cand.queryRank >= rankOutputCount || contextPackObservationCandidate(cand) {
			continue
		}
		if contextPackCandidateDuplicate(cand, out) {
			continue
		}
		out = append(out, cand)
		if len(out) >= anchorCap {
			break
		}
	}
	return out
}

func contextPackRankOutputAnchorCount(cap int) int {
	if cap <= 0 {
		return 0
	}
	count := (cap + contextPackRankOutputAnchorDivisor - 1) / contextPackRankOutputAnchorDivisor
	if count < 1 {
		return 1
	}
	if count > cap {
		return cap
	}
	return count
}

func contextPackFilterAnchorsByRoute(intent domain.QueryIntent, anchors []contextPackCandidate) []contextPackCandidate {
	if len(anchors) == 0 {
		return anchors
	}
	out := anchors[:0]
	for _, cand := range anchors {
		if contextPackAnchorRouteAllowed(intent, cand.hit) {
			out = append(out, cand)
		}
	}
	return out
}

func contextPackAnchorRouteAllowed(intent domain.QueryIntent, hit domain.Hit) bool {
	if contextPackHasRoute(hit, "retrieval") {
		return true
	}
	if contextPackHasRoute(hit, "timeline") {
		return contextPackTemporalIntent(intent)
	}
	if contextPackHasRoute(hit, "relation") || contextPackHasRoute(hit, "assertion") || contextPackHasRoute(hit, "profile") {
		return intent.Subject != "" && (intent.Predicate != "" || intent.Object != "" || len(intent.Kinds) > 0)
	}
	return false
}

func contextPackHasRoute(hit domain.Hit, want string) bool {
	for _, source := range hit.Sources {
		if source == want {
			return true
		}
	}
	return false
}

func contextPackObservationCandidate(cand contextPackCandidate) bool {
	hit := cand.hit
	if hit.Ref.Kind == domain.GraphNodeObservation || hit.Observation.ID != "" {
		return true
	}
	for _, source := range hit.Sources {
		if source == "observation" {
			return true
		}
	}
	return false
}

func newContextPackInput(cap int) contextPackInput {
	return contextPackInput{cap: cap}
}

func newContextPackCandidate(hit domain.Hit, queryRank int, maxHitScore float64, evidenceKey string) contextPackCandidate {
	evidenceText := hitEvidenceText(hit)
	factText := contextPackFactText(hit)
	evidenceTokens := recallintent.TextTokenSet(evidenceText)
	factTokens := recallintent.TextTokenSet(factText)
	candidate := contextPackCandidate{
		hit:            hit,
		baseScore:      hit.Score,
		queryRank:      queryRank,
		evidenceKey:    evidenceKey,
		evidenceGroup:  primaryEvidenceGroup(hit),
		evidenceTokens: evidenceTokens,
		factTokens:     factTokens,
	}
	candidate.score = contextPackScore(candidate, maxHitScore)
	return candidate
}

func contextPackScore(candidate contextPackCandidate, maxHitScore float64) float64 {
	base := 0.0
	if maxHitScore > 0 && candidate.baseScore > 0 {
		base = candidate.baseScore / maxHitScore
		if base > 1 {
			base = 1
		}
	}
	rankPrior := 1 / (1 + float64(candidate.queryRank)/30)
	score := contextPackBaseScoreWeight*base +
		contextPackRankPriorWeight*rankPrior
	if contextPackObservationCandidate(candidate) {
		// Observation hits are raw evidence rescue/support. Their lexical score is
		// not calibrated against structured facts, so keep them from outranking
		// direct assertion/event hits when both are available.
		score = 0.04*base + 0.08*rankPrior
	}
	return score
}

func betterContextPackTieBreak(a, b contextPackCandidate) bool {
	if a.baseScore != b.baseScore {
		return a.baseScore > b.baseScore
	}
	return a.queryRank < b.queryRank
}

func contextPackDiversityPenalty(candidate contextPackCandidate, selected []contextPackCandidate) float64 {
	maxSimilarity := 0.0
	for _, existing := range selected {
		similarity := tokenSetJaccard(candidate.evidenceTokens, existing.evidenceTokens)
		if sameStructuredMemory(candidate.hit.Fact, existing.hit.Fact) {
			similarity += 0.15
		}
		if similarity > maxSimilarity {
			maxSimilarity = similarity
		}
	}
	if maxSimilarity <= contextPackDiversityPenaltyFloor {
		return 0
	}
	penalty := contextPackDiversityPenaltyScale * maxSimilarity
	return penalty
}

func contextPackSourceConcentrationPenalty(candidate contextPackCandidate, selected []contextPackCandidate) float64 {
	source := primaryHitSource(candidate.hit)
	if source == "" {
		return 0
	}
	count := 0
	for _, existing := range selected {
		if primaryHitSource(existing.hit) == source {
			count++
		}
	}
	if count < 2 {
		return 0
	}
	penalty := contextPackSourcePenaltyPerExtra * float64(count-1)
	if penalty > contextPackSourcePenaltyMax {
		return contextPackSourcePenaltyMax
	}
	return penalty
}

func primaryHitSource(hit domain.Hit) string {
	if len(hit.Sources) == 0 {
		return ""
	}
	return hit.Sources[0]
}

func contextPackCandidateDuplicate(candidate contextPackCandidate, selected []contextPackCandidate) bool {
	sameEvidenceCount := 0
	for _, existing := range selected {
		if candidate.hit.Fact.ID != "" && candidate.hit.Fact.ID == existing.hit.Fact.ID {
			return true
		}
		if sameStructuredMemory(candidate.hit.Fact, existing.hit.Fact) && tokenSetJaccard(candidate.evidenceTokens, existing.evidenceTokens) >= contextPackDuplicateJaccardCutoff {
			return true
		}
		if candidate.evidenceKey != "" && candidate.evidenceKey == existing.evidenceKey {
			sameEvidenceCount++
			if !contextPackComplementarySameEvidence(candidate, existing) {
				return true
			}
		}
	}
	if candidate.evidenceKey != "" && sameEvidenceCount >= contextPackSameEvidenceSiblingCap {
		return true
	}
	return false
}

func contextPackComplementarySameEvidence(candidate, existing contextPackCandidate) bool {
	if candidate.hit.Fact.ID == "" || existing.hit.Fact.ID == "" {
		return false
	}
	similarity := tokenSetJaccard(candidate.factTokens, existing.factTokens)
	if similarity <= contextPackSiblingFactJaccardFloor {
		return false
	}
	if sameStructuredMemory(candidate.hit.Fact, existing.hit.Fact) &&
		similarity >= contextPackSiblingFactJaccardCutoff {
		return false
	}
	return true
}

func contextPackHits(candidates []contextPackCandidate) []domain.Hit {
	out := make([]domain.Hit, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.hit)
	}
	return out
}

func contextPackTrace(candidates, selected []contextPackCandidate, rankOutputCount int, dropReasonMaps ...map[string]string) []diagnostic.CandidateSnapshot {
	if len(candidates) == 0 {
		return nil
	}
	var explicitDropReasons map[string]string
	if len(dropReasonMaps) > 0 {
		explicitDropReasons = dropReasonMaps[0]
	}
	out := make([]diagnostic.CandidateSnapshot, 0, len(candidates))
	for _, cand := range candidates {
		contextPackRank := contextPackSelectedRank(cand, selected)
		rankOutputRank := 0
		if cand.queryRank < rankOutputCount {
			rankOutputRank = cand.queryRank + 1
		}
		droppedReason := ""
		if contextPackRank == 0 {
			droppedReason = "capacity"
			if explicitDropReasons != nil {
				if reason := explicitDropReasons[contextPackCandidateTraceKey(cand)]; reason != "" {
					droppedReason = reason
				}
			}
		}
		routes := append([]string(nil), cand.hit.Sources...)
		out = append(out, diagnostic.CandidateSnapshot{
			FactID:           contextPackTraceFactID(cand.hit),
			Source:           primaryHitSource(cand.hit),
			Rank:             cand.queryRank + 1,
			Score:            cand.baseScore,
			EvidenceIDs:      contextPackTraceEvidenceIDs(cand.hit),
			Sources:          routes,
			RankOutputRank:   rankOutputRank,
			ContextPackRank:  contextPackRank,
			PrimarySource:    primaryHitSource(cand.hit),
			ProjectionRoutes: routes,
			DroppedReason:    droppedReason,
		})
	}
	return out
}

func contextPackCandidateTraceKey(cand contextPackCandidate) string {
	if id := contextPackTraceFactID(cand.hit); id != "" {
		return "id:" + id
	}
	if cand.evidenceKey != "" {
		return "ev:" + cand.evidenceKey
	}
	return "rank:" + strconv.Itoa(cand.queryRank)
}

func contextPackSelectedRank(candidate contextPackCandidate, selected []contextPackCandidate) int {
	for i, existing := range selected {
		if sameContextPackCandidate(candidate, existing) {
			return i + 1
		}
	}
	return 0
}

func sameContextPackCandidate(a, b contextPackCandidate) bool {
	if a.hit.Fact.ID != "" && a.hit.Fact.ID == b.hit.Fact.ID {
		return true
	}
	if a.hit.Observation.ID != "" && a.hit.Observation.ID == b.hit.Observation.ID {
		return true
	}
	return false
}

func contextPackTraceFactID(hit domain.Hit) string {
	if hit.Fact.ID != "" {
		return hit.Fact.ID
	}
	if hit.Observation.ID != "" {
		return hit.Observation.ID
	}
	if hit.Link.ID != "" {
		return hit.Link.ID
	}
	return hit.Ref.ID
}

func contextPackTraceEvidenceIDs(hit domain.Hit) []string {
	seen := map[string]struct{}{}
	var out []string
	appendID := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, ref := range hit.Evidence {
		appendID(ref.ID)
		appendID(ref.ObservationID)
		appendID(ref.SpanID)
	}
	for _, ref := range hit.Fact.EvidenceRefs {
		appendID(ref.ID)
		appendID(ref.ObservationID)
		appendID(ref.SpanID)
	}
	if hit.Observation.ID != "" {
		appendID(hit.Observation.ID)
		for _, span := range hit.Observation.Spans {
			appendID(span.ID)
		}
	}
	return out
}

func contextPackFactText(hit domain.Hit) string {
	var b strings.Builder
	for _, part := range []string{
		hit.Fact.Content,
		hit.Fact.Subject,
		hit.Fact.Predicate,
		hit.Fact.Object,
		string(hit.Fact.Kind),
		hit.Fact.Location,
	} {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(part)
	}
	for _, entity := range hit.Fact.Entities {
		entity = strings.TrimSpace(entity)
		if entity == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(entity)
	}
	for _, participant := range hit.Fact.Participants {
		participant = strings.TrimSpace(participant)
		if participant == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(participant)
	}
	if b.Len() == 0 {
		return hitEvidenceText(hit)
	}
	return b.String()
}

func hitEvidenceText(hit domain.Hit) string {
	var b strings.Builder
	evidence := hit.Evidence
	if len(evidence) == 0 {
		evidence = hit.Fact.EvidenceRefs
	}
	for _, ref := range evidence {
		if strings.TrimSpace(ref.Text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(ref.Text)
	}
	if b.Len() == 0 {
		b.WriteString(hit.Fact.EvidenceText)
	}
	return b.String()
}

func primaryEvidenceKey(hit domain.Hit) string {
	evidence := hit.Evidence
	if len(evidence) == 0 {
		evidence = hit.Fact.EvidenceRefs
	}
	for _, ref := range evidence {
		if ref.ID != "" {
			return "id:" + ref.ID
		}
		if ref.MessageID != "" {
			return "msg:" + ref.MessageID
		}
	}
	return ""
}

func primaryEvidenceGroup(hit domain.Hit) string {
	evidence := hit.Evidence
	if len(evidence) == 0 {
		evidence = hit.Fact.EvidenceRefs
	}
	for _, ref := range evidence {
		for _, raw := range []string{ref.ID, ref.MessageID} {
			if group := evidenceGroup(raw); group != "" {
				return group
			}
		}
	}
	return ""
}

func evidenceGroup(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ';' || r == ',' || r == ' '
	})
	if len(parts) == 0 {
		return ""
	}
	raw = parts[0]
	idx := strings.LastIndex(raw, ":")
	if idx <= 0 || idx == len(raw)-1 {
		return ""
	}
	if _, err := strconv.Atoi(raw[idx+1:]); err != nil {
		return ""
	}
	return raw[:idx]
}

func sameStructuredMemory(a, b domain.TemporalFact) bool {
	if a.Subject != "" && b.Subject != "" && !strings.EqualFold(a.Subject, b.Subject) {
		return false
	}
	if a.Predicate != "" && b.Predicate != "" && !strings.EqualFold(a.Predicate, b.Predicate) {
		return false
	}
	return a.Kind == b.Kind
}

func tokenSetJaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	intersect := 0
	for tok := range a {
		if _, ok := b[tok]; ok {
			intersect++
		}
	}
	union := len(a) + len(b) - intersect
	if union == 0 {
		return 0
	}
	return float64(intersect) / float64(union)
}
