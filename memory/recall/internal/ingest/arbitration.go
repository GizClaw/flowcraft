package ingest

import (
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

type arbitrationResult struct {
	Winners   []groundedProposal
	Losers    []arbitrationLoser
	Decisions []arbitrationDecision
}

type arbitrationDecision struct {
	Key              string
	WinnerID         string
	LoserIDs         []string
	Reason           string
	WinningFamily    semanticProposalFamily
	LosingFamilies   []semanticProposalFamily
	SupportSpanIDs   []string
	ParameterSlotKey string
}

type arbitrationLoser struct {
	Proposal groundedProposal
	Reason   string
	Key      string
}

func proposalFamilyPriority(family semanticProposalFamily) int {
	switch family {
	case proposalFamilyParameter:
		return 4
	case proposalFamilyProcedure:
		return 3
	case proposalFamilyIntentPlan:
		return 2
	case proposalFamilySemanticFact:
		return 1
	default:
		return 0
	}
}

func groundedProposalMeaningKey(candidate groundedProposal) string {
	if candidate.Family == proposalFamilyParameter {
		if key := groundedParameterMeaningKey(candidate, true); key != "" {
			return "parameter_full\x00" + key
		}
	}
	if candidate.Proposal.Semantic != nil {
		return groundedSemanticMeaningKey(candidate)
	}
	return ""
}

func groundedParameterMeaningKey(grounded groundedProposal, includeIdentity bool) string {
	if len(grounded.SupportRefs) == 0 {
		return ""
	}
	ref := grounded.SupportRefs[0]
	if ref.ObservationID == "" || ref.SpanID == "" {
		return ""
	}
	value := strings.ToLower(strings.TrimSpace(grounded.Normalized.NormalizedValue))
	if value == "" {
		return ""
	}
	parts := []string{
		ref.ObservationID,
		ref.SpanID,
	}
	if includeIdentity {
		parts = append(parts,
			strings.ToLower(strings.TrimSpace(grounded.Normalized.Owner)),
			strings.ToLower(strings.TrimSpace(grounded.Normalized.NamespacePath)),
		)
	}
	parts = append(parts,
		strings.ToLower(strings.TrimSpace(grounded.Normalized.CanonicalName)),
	)
	if includeIdentity {
		parts = append(parts, strings.ToLower(strings.TrimSpace(grounded.Normalized.ValueKind)))
	}
	parts = append(parts, value, grounded.Normalized.ConditionIdentity)
	return strings.Join(parts, "\x00")
}

func groundedSemanticMeaningKey(grounded groundedProposal) string {
	if len(grounded.SupportRefs) == 0 {
		return ""
	}
	ref := grounded.SupportRefs[0]
	if ref.ObservationID == "" || ref.SpanID == "" {
		return ""
	}
	proposal := grounded.Proposal.Semantic
	if proposal == nil {
		return ""
	}
	content := normalizeEvidenceQuote(grounded.SemanticContent)
	if content == "" {
		return ""
	}
	return strings.Join([]string{
		"semantic",
		string(grounded.Family),
		ref.ObservationID,
		ref.SpanID,
		strings.ToLower(strings.TrimSpace(proposal.Kind)),
		strings.ToLower(strings.TrimSpace(proposal.Subject)),
		strings.ToLower(strings.TrimSpace(proposal.Predicate)),
		strings.ToLower(strings.TrimSpace(proposal.Object)),
		content,
	}, "\x00")
}

func arbitrateGroundedProposals(grounded []groundedProposal) arbitrationResult {
	ordered := append([]groundedProposal(nil), grounded...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return proposalFamilyPriority(ordered[i].Family) > proposalFamilyPriority(ordered[j].Family)
	})
	owned := map[string]int{}
	result := arbitrationResult{Winners: make([]groundedProposal, 0, len(ordered))}
	for _, candidate := range ordered {
		keys := groundedProposalLookupKeys(candidate)
		if winnerIndex, key, dup := firstOwnedKey(owned, keys); dup {
			winner := result.Winners[winnerIndex]
			result.Losers = append(result.Losers, arbitrationLoser{Proposal: candidate, Reason: "duplicate_by_arbitration", Key: key})
			result.Decisions = append(result.Decisions, arbitrationDecision{
				Key:            key,
				WinnerID:       winner.ProposalID,
				LoserIDs:       []string{candidate.ProposalID},
				Reason:         "duplicate_by_arbitration",
				WinningFamily:  winner.Family,
				LosingFamilies: []semanticProposalFamily{candidate.Family},
				SupportSpanIDs: supportSpanIDs(candidate),
			})
			continue
		}
		for _, key := range groundedProposalRegistrationKeys(candidate) {
			owned[key] = len(result.Winners)
		}
		result.Winners = append(result.Winners, candidate)
	}
	return result
}

func groundedProposalLookupKeys(candidate groundedProposal) []string {
	var keys []string
	if candidate.Family == proposalFamilyParameter {
		if key := groundedParameterMeaningKey(candidate, true); key != "" {
			keys = append(keys, "parameter_full\x00"+key)
		}
		return keys
	}
	key := groundedProposalMeaningKey(candidate)
	if key != "" {
		keys = append(keys, key)
	}
	if key := groundedSemanticParameterOverlapKey(candidate); key != "" {
		keys = append(keys, key)
	}
	return keys
}

func groundedProposalRegistrationKeys(candidate groundedProposal) []string {
	keys := groundedProposalLookupKeys(candidate)
	if candidate.Family == proposalFamilyParameter {
		keys = append(keys, groundedParameterSemanticOverlapKeys(candidate)...)
	}
	return keys
}

func groundedParameterSemanticOverlapKeys(candidate groundedProposal) []string {
	if candidate.Family != proposalFamilyParameter || len(candidate.SupportRefs) == 0 {
		return nil
	}
	ref := candidate.SupportRefs[0]
	if ref.ObservationID == "" || ref.SpanID == "" {
		return nil
	}
	value := strings.ToLower(strings.TrimSpace(candidate.Normalized.NormalizedValue))
	if value == "" {
		return nil
	}
	key := strings.Join([]string{
		"parameter_semantic_overlap",
		ref.ObservationID,
		ref.SpanID,
		strings.ToLower(strings.TrimSpace(candidate.Normalized.Owner)),
		strings.ToLower(strings.TrimSpace(candidate.Normalized.NamespacePath)),
		strings.ToLower(strings.TrimSpace(candidate.Normalized.CanonicalName)),
		strings.ToLower(strings.TrimSpace(candidate.Normalized.ValueKind)),
		value,
		candidate.Normalized.ConditionIdentity,
	}, "\x00")
	return []string{key}
}

func groundedSemanticParameterOverlapKey(candidate groundedProposal) string {
	if candidate.Family != proposalFamilySemanticFact || candidate.SemanticBinding.ParameterOverlap == nil || len(candidate.SupportRefs) == 0 {
		return ""
	}
	ref := candidate.SupportRefs[0]
	if ref.ObservationID == "" || ref.SpanID == "" {
		return ""
	}
	overlap := candidate.SemanticBinding.ParameterOverlap
	return strings.Join([]string{
		"parameter_semantic_overlap",
		ref.ObservationID,
		ref.SpanID,
		strings.ToLower(strings.TrimSpace(overlap.Owner)),
		strings.ToLower(strings.TrimSpace(overlap.NamespacePath)),
		strings.ToLower(strings.TrimSpace(overlap.CanonicalName)),
		strings.ToLower(strings.TrimSpace(overlap.ValueKind)),
		strings.ToLower(strings.TrimSpace(overlap.NormalizedValue)),
		overlap.ConditionIdentity,
	}, "\x00")
}

func firstOwnedKey(owned map[string]int, keys []string) (int, string, bool) {
	for _, key := range keys {
		if idx, ok := owned[key]; ok {
			return idx, key, true
		}
	}
	return 0, "", false
}

func supportSpanIDs(candidate groundedProposal) []string {
	if len(candidate.Normalized.SupportSpanIDs) > 0 {
		return append([]string(nil), candidate.Normalized.SupportSpanIDs...)
	}
	out := make([]string, 0, len(candidate.SupportRefs)+len(candidate.ConfirmationRefs))
	for _, ref := range candidate.SupportRefs {
		if ref.SpanID != "" {
			out = append(out, ref.SpanID)
		}
	}
	for _, ref := range candidate.ConfirmationRefs {
		if ref.SpanID != "" {
			out = append(out, ref.SpanID)
		}
	}
	return out
}

func semanticFactDedupeSet(existing []domain.TemporalFact) map[string]struct{} {
	seen := make(map[string]struct{}, len(existing))
	for _, fact := range existing {
		if key := semanticFactDedupeKey(fact); key != "" {
			seen[key] = struct{}{}
		}
	}
	return seen
}

func markSemanticFactProposalSeen(seen map[string]struct{}, fact domain.TemporalFact) bool {
	key := semanticFactDedupeKey(fact)
	if key == "" {
		return true
	}
	if _, dup := seen[key]; dup {
		return false
	}
	seen[key] = struct{}{}
	return true
}

func semanticFactDedupeKey(fact domain.TemporalFact) string {
	content := normalizeEvidenceQuote(fact.Content)
	if content == "" {
		return ""
	}
	ids := sourceIDsFromEvidence(fact.EvidenceRefs)
	if len(ids) == 0 {
		ids = append([]string(nil), fact.SourceMessageIDs...)
	}
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}
	sort.Strings(ids)
	ids = compactNonEmptyStrings(ids)
	return string(fact.Kind) + "\x00" + content + "\x00" + strings.Join(ids, "\x00")
}

func compactNonEmptyStrings(in []string) []string {
	out := in[:0]
	var last string
	for _, s := range in {
		if s == "" || s == last {
			continue
		}
		out = append(out, s)
		last = s
	}
	return out
}
