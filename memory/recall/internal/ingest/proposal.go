package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

type semanticProposalFamily string

const (
	proposalFamilySemanticFact semanticProposalFamily = "semantic_fact"
	proposalFamilyParameter    semanticProposalFamily = "parameter_slot"
	proposalFamilyProcedure    semanticProposalFamily = "procedure_step"
	proposalFamilyIntentPlan   semanticProposalFamily = "intent_plan"
)

type proposalCandidate struct {
	ID          string
	Family      semanticProposalFamily
	SourceSpans []domain.SourceEvidenceSpan
	Semantic    *SemanticFactProposal
	Parameter   *ParameterProposal
}

func proposalFamily(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "semantic_fact":
		return "semantic_fact"
	case "parameter_slot":
		return "parameter_slot"
	case "procedure_step":
		return "procedure_step"
	case "intent_plan":
		return "intent_plan"
	default:
		return ""
	}
}

func activeProposalFamily(family semanticProposalFamily) bool {
	switch family {
	case proposalFamilySemanticFact, proposalFamilyParameter:
		return true
	default:
		return false
	}
}

func assignProposalIDs(proposals []proposalCandidate) []proposalCandidate {
	for i := range proposals {
		if strings.TrimSpace(proposals[i].ID) != "" {
			continue
		}
		proposals[i].ID = proposalCandidateID(proposals[i], i)
	}
	return proposals
}

func proposalCandidateID(p proposalCandidate, index int) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%d", p.Family, index)
	for _, span := range p.SourceSpans {
		_, _ = fmt.Fprintf(h, "\x00%s:%s:%s", span.ObservationID, span.SpanID, span.SourceID)
	}
	if p.Parameter != nil {
		_, _ = fmt.Fprintf(h, "\x00parameter:%s:%s:%s:%s:%s", p.Parameter.Owner, p.Parameter.NameSurface, p.Parameter.ValueSurface, p.Parameter.Quote, strings.Join(p.Parameter.SourceIDs, ","))
	}
	if p.Semantic != nil {
		_, _ = fmt.Fprintf(h, "\x00semantic:%s:%s:%s:%s:%s", p.Semantic.Kind, p.Semantic.Subject, p.Semantic.Predicate, p.Semantic.Object, p.Semantic.Quote)
	}
	sum := h.Sum(nil)
	return "proposal_" + hex.EncodeToString(sum[:8])
}
