package stages

import (
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
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

func candidateExpansionSuggestionSetCandidate(structuralSetSibling bool) bool {
	return structuralSetSibling
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

func candidateExpansionSuggestionBridgeCandidate(item domain.ContextItem, seeds []domain.ContextItem) bool {
	group := contextItemEvidenceGroup(item)
	for _, seed := range seeds {
		if group != "" && group == contextItemEvidenceGroup(seed) {
			return true
		}
	}
	return false
}

func contextItemEvidenceGroup(item domain.ContextItem) string {
	refs := item.Evidence
	if len(refs) == 0 {
		refs = item.Fact.EvidenceRefs
	}
	for _, ref := range refs {
		if group := evidenceRefSourceGroup(ref); group != "" {
			return group
		}
	}
	return ""
}
