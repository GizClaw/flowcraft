package ingest

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

type compiledFactDecision struct {
	ProposalID string
	Family     semanticProposalFamily
	Accepted   bool
	Reason     string
	Fact       domain.TemporalFact
}

func compileTemporalFacts(ctx context.Context, promotions []promotionDecision, turnIndex map[string]port.TurnContext) ([]domain.TemporalFact, []compiledFactDecision) {
	out := make([]domain.TemporalFact, 0, len(promotions))
	decisions := make([]compiledFactDecision, 0, len(promotions))
	for _, promotion := range promotions {
		fact, ok, reason := compileTemporalFact(promotion, turnIndex)
		decision := compiledFactDecision{
			ProposalID: promotion.ProposalID,
			Family:     promotion.Family,
			Accepted:   ok,
			Reason:     reason,
			Fact:       fact,
		}
		if !ok {
			decisions = append(decisions, decision)
			recordRejectedGroundedProposal(ctx, promotion.Grounded, reason)
			continue
		}
		if !factEvidenceWithinSourceTurns(fact, turnIndex) {
			decision.Accepted = false
			decision.Reason = "evidence_outside_source_turns"
			decisions = append(decisions, decision)
			recordRejectedGroundedProposal(ctx, promotion.Grounded, decision.Reason)
			continue
		}
		recordExtractorCandidateAccepted(ctx, string(promotion.Family))
		out = append(out, fact)
		decisions = append(decisions, decision)
	}
	return out, decisions
}

func compileTemporalFact(promotion promotionDecision, turnIndex map[string]port.TurnContext) (domain.TemporalFact, bool, string) {
	switch promotion.Family {
	case proposalFamilyParameter:
		return compileParameterTemporalFact(promotion), true, ""
	default:
		if promotion.Grounded.Proposal.Semantic == nil {
			return domain.TemporalFact{}, false, "unsupported_schema"
		}
		return compileSemanticTemporalFact(promotion, turnIndex), true, ""
	}
}

func compileSemanticTemporalFact(promotion promotionDecision, turnIndex map[string]port.TurnContext) domain.TemporalFact {
	grounded := promotion.Grounded
	m := grounded.Proposal.Semantic
	fact := domain.TemporalFact{
		Content:      grounded.SemanticContent,
		EvidenceText: evidenceTextFromRefs(appendEvidenceRefs(grounded.SupportRefs, grounded.ConfirmationRefs), turnIndex),
		Kind:         deterministicSemanticKind(grounded.Family),
		Subject:      groundedSemanticField(grounded.SemanticContent, m.Subject),
		Predicate:    groundedSemanticField(grounded.SemanticContent, m.Predicate),
		Object:       groundedSemanticField(grounded.SemanticContent, m.Object),
		Entities:     groundedSemanticEntities(grounded.SemanticContent, m.Entities),
		EvidenceRefs: appendEvidenceRefs(grounded.SupportRefs, grounded.ConfirmationRefs),
	}
	fact.SourceMessageIDs = sourceIDsFromEvidence(fact.EvidenceRefs)
	return fact
}

func groundedSemanticField(content, surface string) string {
	surface = strings.TrimSpace(surface)
	if surface == "" || !containsSurface(content, surface) {
		return ""
	}
	return surface
}

func groundedSemanticEntities(content string, entities []string) []string {
	out := make([]string, 0, len(entities))
	seen := map[string]struct{}{}
	for _, entity := range entities {
		entity = strings.TrimSpace(entity)
		if entity == "" || !containsSurface(content, entity) {
			continue
		}
		key := strings.ToLower(entity)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, entity)
	}
	return out
}

func appendEvidenceRefs(primary, secondary []domain.EvidenceRef) []domain.EvidenceRef {
	out := append([]domain.EvidenceRef(nil), primary...)
	out = append(out, secondary...)
	return out
}

func factEvidenceWithinSourceTurns(f domain.TemporalFact, turnIndex map[string]port.TurnContext) bool {
	if len(turnIndex) == 0 {
		return true
	}
	if len(f.EvidenceRefs) == 0 {
		return false
	}
	for _, ref := range f.EvidenceRefs {
		if !evidenceRefWithinSourceTurns(ref, turnIndex) {
			return false
		}
	}
	for _, id := range f.SourceMessageIDs {
		trimmed := strings.TrimSpace(id)
		if _, ok := turnIndex[trimmed]; !ok && !sourceIDCoveredByCanonicalRef(trimmed, f.EvidenceRefs) {
			return false
		}
	}
	return true
}

func evidenceRefWithinSourceTurns(ref domain.EvidenceRef, turnIndex map[string]port.TurnContext) bool {
	if strings.TrimSpace(ref.ObservationID) != "" && strings.TrimSpace(ref.SpanID) != "" {
		return true
	}
	checked := false
	var (
		primaryTurn port.TurnContext
		havePrimary bool
	)
	if id := strings.TrimSpace(ref.ID); id != "" {
		checked = true
		turn, ok := turnIndex[id]
		if !ok {
			return false
		}
		primaryTurn = turn
		havePrimary = true
	}
	if id := strings.TrimSpace(ref.MessageID); id != "" {
		checked = true
		turn, ok := turnIndex[id]
		if !ok {
			return false
		}
		if havePrimary && sourceTurnIdentity(primaryTurn) != sourceTurnIdentity(turn) {
			return false
		}
	}
	return checked
}

func sourceIDCoveredByCanonicalRef(sourceID string, refs []domain.EvidenceRef) bool {
	if sourceID == "" {
		return false
	}
	for _, ref := range refs {
		if strings.TrimSpace(ref.ObservationID) == "" || strings.TrimSpace(ref.SpanID) == "" {
			continue
		}
		if strings.TrimSpace(ref.ID) == sourceID || strings.TrimSpace(ref.MessageID) == sourceID {
			return true
		}
	}
	return false
}

func sourceTurnIdentity(turn port.TurnContext) string {
	if id := strings.TrimSpace(turn.EvidenceID); id != "" {
		return id
	}
	return strings.TrimSpace(turn.ID)
}

func evidenceTextFromRefs(refs []domain.EvidenceRef, turnIndex map[string]port.TurnContext) string {
	if len(refs) == 0 {
		return ""
	}
	seen := make(map[string]struct{}, len(refs))
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		text := strings.TrimSpace(evidenceSourceText(ref, turnIndex))
		if text == "" {
			continue
		}
		key := normalizeEvidenceQuote(text)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n")
}

func evidenceSourceText(ref domain.EvidenceRef, turnIndex map[string]port.TurnContext) string {
	if turn, ok := lookupEvidenceTurn(ref, turnIndex); ok && strings.TrimSpace(turn.Text) != "" {
		if quote := strings.TrimSpace(ref.Text); quote != "" && turnContainsQuote(turn, quote) {
			return quote
		}
		return turn.Text
	}
	return ref.Text
}

func lookupEvidenceTurn(ref domain.EvidenceRef, turnIndex map[string]port.TurnContext) (port.TurnContext, bool) {
	if len(turnIndex) == 0 {
		return port.TurnContext{}, false
	}
	if turn, ok := turnIndex[ref.ID]; ok {
		return turn, true
	}
	if turn, ok := turnIndex[ref.MessageID]; ok {
		return turn, true
	}
	return port.TurnContext{}, false
}

func hasEvidenceID(refs []domain.EvidenceRef) bool {
	for _, ref := range refs {
		if strings.TrimSpace(ref.ID) != "" {
			return true
		}
	}
	return false
}

func turnContainsQuote(turn port.TurnContext, quote string) bool {
	_, ok := turnQuoteSpan(turn, quote)
	return ok
}

func turnQuoteSpan(turn port.TurnContext, quote string) (string, bool) {
	text := turn.Text
	quote = strings.TrimSpace(quote)
	if strings.TrimSpace(text) == "" || quote == "" {
		return "", false
	}
	if idx := strings.Index(text, quote); idx >= 0 {
		return text[idx : idx+len(quote)], true
	}
	return tokenEquivalentQuoteSpan(text, quote)
}

func sourceIDsFromEvidence(refs []domain.EvidenceRef) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.ID == "" {
			continue
		}
		out = append(out, r.ID)
	}
	return out
}
