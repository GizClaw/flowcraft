package stages

import (
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	recallintent "github.com/GizClaw/flowcraft/memory/recall/internal/intent"
)

const (
	candidateExpansionSuggestionTextFloor = 0.14
)

func hasExpansionTask(tasks []domain.QueryTaskIntent) bool {
	return hasTask(tasks, domain.QueryTaskSetCompletion) || hasTask(tasks, domain.QueryTaskBridgeResolution)
}

func hasTask(tasks []domain.QueryTaskIntent, want domain.QueryTaskIntent) bool {
	for _, task := range tasks {
		if task == want {
			return true
		}
	}
	return false
}

func candidateExpansionSuggestionSeeds(items []domain.ContextItem, cap int) []domain.ContextItem {
	if cap <= 0 || cap > len(items) {
		cap = len(items)
	}
	out := make([]domain.ContextItem, cap)
	copy(out, items[:cap])
	return out
}

func candidateExpansionSuggestionSeedContains(seeds []domain.ContextItem, factID string) bool {
	if factID == "" {
		return false
	}
	for _, seed := range seeds {
		if seed.Fact.ID == factID {
			return true
		}
	}
	return false
}

func candidateExpansionSuggestionSetCandidate(features domain.QueryFeatures, item domain.ContextItem, seeds []domain.ContextItem, covered map[string]struct{}, structuralSetSibling bool) bool {
	if candidateExpansionSuggestionCoverageGain(features, item, covered) > 0 {
		return true
	}
	if structuralSetSibling {
		return true
	}
	return false
}

func candidateExpansionSuggestionHasSetSibling(item domain.ContextItem, seeds []domain.ContextItem) bool {
	for _, seed := range seeds {
		if sameSubjectPredicate(item.Fact, seed.Fact) {
			return true
		}
	}
	return false
}

func sameSubjectPredicate(a, b domain.TemporalFact) bool {
	return strings.TrimSpace(a.Subject) != "" &&
		strings.TrimSpace(b.Subject) != "" &&
		strings.EqualFold(a.Subject, b.Subject) &&
		strings.TrimSpace(a.Predicate) != "" &&
		strings.TrimSpace(b.Predicate) != "" &&
		strings.EqualFold(a.Predicate, b.Predicate)
}

func candidateExpansionSuggestionBridgeCandidate(features domain.QueryFeatures, item domain.ContextItem, seeds []domain.ContextItem, covered map[string]struct{}) bool {
	if candidateExpansionSuggestionCoverageGain(features, item, covered) <= 0 {
		return false
	}
	group := contextItemEvidenceGroup(item)
	for _, seed := range seeds {
		if group != "" && group == contextItemEvidenceGroup(seed) {
			return true
		}
		if collectionSiblingFacts(item.Fact, seed.Fact) {
			return true
		}
	}
	return false
}

func candidateExpansionSuggestionCoveredTokens(features domain.QueryFeatures, items []domain.ContextItem) map[string]struct{} {
	covered := map[string]struct{}{}
	for _, item := range items {
		tokens := candidateExpansionSuggestionTokenSet(item)
		for tok := range features.Tokens {
			if _, ok := tokens[tok]; ok {
				covered[tok] = struct{}{}
			}
		}
	}
	return covered
}

func candidateExpansionSuggestionCoverageGain(features domain.QueryFeatures, item domain.ContextItem, covered map[string]struct{}) float64 {
	if len(features.Tokens) == 0 {
		return 0
	}
	tokens := candidateExpansionSuggestionTokenSet(item)
	newMatches := 0
	for tok := range features.Tokens {
		if _, ok := covered[tok]; ok {
			continue
		}
		if _, ok := tokens[tok]; ok {
			newMatches++
		}
	}
	return float64(newMatches) / float64(len(features.Tokens))
}

func candidateExpansionSuggestionTextScore(features domain.QueryFeatures, item domain.ContextItem) float64 {
	if len(features.Tokens) == 0 {
		return 0
	}
	tokens := candidateExpansionSuggestionTokenSet(item)
	matched := 0
	for tok := range features.Tokens {
		if _, ok := tokens[tok]; ok {
			matched++
		}
	}
	return float64(matched) / float64(len(features.Tokens))
}

func candidateExpansionSuggestionTokenSet(item domain.ContextItem) map[string]struct{} {
	var b strings.Builder
	appendPart := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(s)
	}
	appendPart(item.Fact.Content)
	appendPart(item.Fact.Subject)
	appendPart(item.Fact.Predicate)
	appendPart(item.Fact.Object)
	appendPart(item.Fact.Location)
	for _, entity := range item.Fact.Entities {
		appendPart(entity)
	}
	for _, participant := range item.Fact.Participants {
		appendPart(participant)
	}
	for _, ref := range item.Evidence {
		appendPart(ref.Text)
	}
	if len(item.Evidence) == 0 {
		for _, ref := range item.Fact.EvidenceRefs {
			appendPart(ref.Text)
		}
	}
	return recallintent.TextTokenSet(b.String())
}

func contextItemEvidenceGroup(item domain.ContextItem) string {
	refs := item.Evidence
	if len(refs) == 0 {
		refs = item.Fact.EvidenceRefs
	}
	for _, ref := range refs {
		for _, raw := range []string{ref.ID, ref.MessageID} {
			if group := evidenceGroup(raw); group != "" {
				return group
			}
		}
	}
	return ""
}
