package ingest

import "context"

type promotionResult struct {
	Accepted []promotionDecision
	Rejected []promotionDecision
}

type promotionDecision struct {
	ProposalID     string
	Family         semanticProposalFamily
	Accepted       bool
	Reason         string
	GroundingLevel groundingLevel
	Grounded       groundedProposal
}

func promoteProposals(ctx context.Context, winners []groundedProposal) promotionResult {
	result := promotionResult{Accepted: make([]promotionDecision, 0, len(winners))}
	for _, candidate := range winners {
		decision := promotionDecision{
			ProposalID:     candidate.ProposalID,
			Family:         candidate.Family,
			Accepted:       true,
			GroundingLevel: candidate.Level,
			Grounded:       candidate,
		}
		if !promotionGroundingAllowed(candidate.Level) {
			decision.Accepted = false
			decision.Reason = "grounding_level_not_promotable"
			result.Rejected = append(result.Rejected, decision)
			recordRejectedGroundedProposal(ctx, candidate, decision.Reason)
			continue
		}
		if !promotionFamilyAllowed(candidate.Family) {
			decision.Accepted = false
			decision.Reason = "reserved_family"
			result.Rejected = append(result.Rejected, decision)
			recordRejectedGroundedProposal(ctx, candidate, decision.Reason)
			continue
		}
		result.Accepted = append(result.Accepted, decision)
	}
	return result
}

func promotionFamilyAllowed(family semanticProposalFamily) bool {
	return activeProposalFamily(family)
}

func promotionGroundingAllowed(level groundingLevel) bool {
	switch level {
	case groundingExact, groundingNormalized, groundingComposed, groundingDialogueConfirmed:
		return true
	default:
		return false
	}
}
