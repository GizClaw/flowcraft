package ingest

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

type extractorGuardContextKey struct{}

type extractorGuardAccumulator struct {
	mu  sync.Mutex
	out diagnostic.ExtractorGuard
}

func newExtractorGuardAccumulator() *extractorGuardAccumulator {
	return &extractorGuardAccumulator{}
}

func withExtractorGuardAccumulator(ctx context.Context, acc *extractorGuardAccumulator) context.Context {
	if acc == nil {
		return ctx
	}
	return context.WithValue(ctx, extractorGuardContextKey{}, acc)
}

func recordExtractorCandidateAccepted(ctx context.Context, family string) {
	acc, _ := ctx.Value(extractorGuardContextKey{}).(*extractorGuardAccumulator)
	if acc == nil {
		return
	}
	acc.addAccepted(family)
}

func recordExtractorCandidateRejected(ctx context.Context, fact diagnostic.GuardedSemanticProposal) {
	acc, _ := ctx.Value(extractorGuardContextKey{}).(*extractorGuardAccumulator)
	if acc == nil {
		return
	}
	acc.addRejected(fact)
}

func recordRejectedProposal(ctx context.Context, proposal proposalCandidate, reason string) {
	if proposal.Parameter != nil {
		recordExtractorCandidateRejected(ctx, guardedParameterProposal(*proposal.Parameter, reason))
		return
	}
	if proposal.Semantic != nil {
		recordExtractorCandidateRejected(ctx, guardedSemanticFactProposalForFamily(proposal.Family, *proposal.Semantic, reason))
		return
	}
	recordExtractorCandidateRejected(ctx, diagnostic.GuardedSemanticProposal{
		Family:      string(proposal.Family),
		Kind:        string(proposal.Family),
		GuardReason: reason,
	})
}

func recordRejectedGroundedProposal(ctx context.Context, candidate groundedProposal, reason string) {
	recordRejectedProposal(ctx, candidate.Proposal, reason)
}

func buildProposalLifecycleDetail(proposals []proposalCandidate, grounding groundingResult, arbitration arbitrationResult, promotion promotionResult, compileDecisions []compiledFactDecision) diagnostic.ProposalLifecycleDetail {
	out := diagnostic.ProposalLifecycleDetail{ByFamily: map[string]diagnostic.ProposalFamilyLifecycle{}}
	addFamily := func(family semanticProposalFamily, fn func(row *diagnostic.ProposalFamilyLifecycle)) {
		key := string(family)
		if key == "" {
			key = "unknown"
		}
		row := out.ByFamily[key]
		fn(&row)
		out.ByFamily[key] = row
	}
	for _, proposal := range proposals {
		addFamily(proposal.Family, func(row *diagnostic.ProposalFamilyLifecycle) {
			row.Proposed++
		})
	}
	for _, grounded := range grounding.Accepted {
		addFamily(grounded.Family, func(row *diagnostic.ProposalFamilyLifecycle) {
			row.Grounded++
		})
		incrementStringCount(&out.Grounding.ByLevel, string(grounded.Level))
	}
	for _, rejected := range grounding.Rejected {
		addFamily(rejected.Family, func(row *diagnostic.ProposalFamilyLifecycle) {
			row.Rejected++
		})
		incrementStringCount(&out.Grounding.RejectReasons, rejected.RejectReason)
	}
	out.Grounding.Input = len(proposals)
	out.Grounding.Accepted = len(grounding.Accepted)
	out.Grounding.Rejected = len(grounding.Rejected)

	out.Arbitration.Input = len(grounding.Accepted)
	out.Arbitration.Winners = len(arbitration.Winners)
	out.Arbitration.Losers = len(arbitration.Losers)
	for _, loser := range arbitration.Losers {
		incrementStringCount(&out.Arbitration.RejectReasons, loser.Reason)
		addFamily(loser.Proposal.Family, func(row *diagnostic.ProposalFamilyLifecycle) {
			row.Rejected++
		})
	}

	out.Promotion.Input = len(arbitration.Winners)
	out.Promotion.Accepted = len(promotion.Accepted)
	out.Promotion.Rejected = len(promotion.Rejected)
	for _, rejected := range promotion.Rejected {
		incrementStringCount(&out.Promotion.RejectReasons, rejected.Reason)
		addFamily(rejected.Family, func(row *diagnostic.ProposalFamilyLifecycle) {
			row.Rejected++
		})
	}

	out.Compile.Input = len(promotion.Accepted)
	for _, decision := range compileDecisions {
		if decision.Accepted {
			out.Compile.Compiled++
			addFamily(decision.Family, func(row *diagnostic.ProposalFamilyLifecycle) {
				row.Promoted++
			})
			continue
		}
		out.Compile.Rejected++
		incrementStringCount(&out.Compile.RejectReasons, decision.Reason)
		addFamily(decision.Family, func(row *diagnostic.ProposalFamilyLifecycle) {
			row.Rejected++
		})
	}
	return out
}

func incrementStringCount(dst *map[string]int, key string) {
	if key == "" {
		key = "unknown"
	}
	if *dst == nil {
		*dst = map[string]int{}
	}
	(*dst)[key]++
}

func (a *extractorGuardAccumulator) addAccepted(family string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.out.Candidates++
	a.out.Accepted++
	a.addFamilyLocked(family, true)
}

func (a *extractorGuardAccumulator) addRejected(fact diagnostic.GuardedSemanticProposal) {
	if a == nil {
		return
	}
	if fact.GuardReason == "" {
		fact.GuardReason = "unknown"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.out.Candidates++
	a.out.Rejected++
	a.addFamilyLocked(firstNonEmpty(fact.Family, fact.Kind), false)
	if a.out.ByReason == nil {
		a.out.ByReason = map[string]int{}
	}
	a.out.ByReason[fact.GuardReason]++
	a.out.RejectedProposals = append(a.out.RejectedProposals, fact)
}

func (a *extractorGuardAccumulator) addFamilyLocked(family string, accepted bool) {
	if family == "" {
		family = "unknown"
	}
	if a.out.ByFamily == nil {
		a.out.ByFamily = map[string]diagnostic.FamilyGuard{}
	}
	row := a.out.ByFamily[family]
	row.Candidates++
	if accepted {
		row.Accepted++
	} else {
		row.Rejected++
	}
	a.out.ByFamily[family] = row
}

func (a *extractorGuardAccumulator) snapshot() diagnostic.ExtractorGuard {
	if a == nil {
		return diagnostic.ExtractorGuard{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.out
	if len(a.out.ByReason) > 0 {
		out.ByReason = make(map[string]int, len(a.out.ByReason))
		for k, v := range a.out.ByReason {
			out.ByReason[k] = v
		}
	}
	if len(a.out.ByFamily) > 0 {
		out.ByFamily = make(map[string]diagnostic.FamilyGuard, len(a.out.ByFamily))
		for k, v := range a.out.ByFamily {
			out.ByFamily[k] = v
		}
	}
	if len(a.out.RejectedProposals) > 0 {
		out.RejectedProposals = append([]diagnostic.GuardedSemanticProposal(nil), a.out.RejectedProposals...)
	}
	return out
}
