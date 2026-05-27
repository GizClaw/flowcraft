package stages

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
)

// sensitivityRank orders labels low → high for MaxSensitivity checks.
var sensitivityRank = map[string]int{
	"public":   0,
	"internal": 1,
	"private":  2,
	"secret":   3,
}

// PolicyFilter enforces Query.Trust at read time. It runs after
// candidate_merge_and_materialize and before rank.
type PolicyFilter struct{}

// NewPolicyFilter constructs a PolicyFilter stage.
func NewPolicyFilter() *PolicyFilter { return &PolicyFilter{} }

// Name implements pipeline.Stage.
func (PolicyFilter) Name() string { return "policy_filter" }

// Skip implements pipeline.Conditional.
func (s *PolicyFilter) Skip(_ context.Context, state *read.ReadState) (bool, diagnostic.StageDetail) {
	if state == nil || state.Query.Trust == nil {
		read.PromoteMergedItems(state)
		state.AfterTrust = state.MergedItems
		detail := diagnostic.PolicyFilterDetail{}
		if snapshotsEnabled(state) {
			detail.Items = candidateSnapshotPtr(contextItemSnapshots(state.AfterTrust))
		}
		return true, detail
	}
	return false, nil
}

// Run implements pipeline.Stage.
func (s *PolicyFilter) Run(_ context.Context, state *read.ReadState) (diagnostic.StageDetail, error) {
	_ = s
	read.PromoteMergedItems(state)
	trust := state.Query.Trust
	maxRank := sensitivityRank[strings.ToLower(trust.MaxSensitivity)]
	allowedScopes := trustScopes(trust)

	var kept []domain.ContextItem
	detail := diagnostic.PolicyFilterDetail{
		MaxSensitivity: trust.MaxSensitivity,
		ActorID:        trust.ActorID,
	}
	for _, item := range state.MergedItems {
		f := item.Fact
		if trust.ActorID != "" && f.Scope.AgentID != "" && f.Scope.AgentID != trust.ActorID {
			detail.Removed++
			continue
		}
		if len(allowedScopes) > 0 && !scopeAllowed(f.Scope, allowedScopes) {
			detail.Removed++
			continue
		}
		label := factSensitivity(f)
		if trust.MaxSensitivity != "" && sensitivityRank[label] > maxRank {
			detail.Removed++
			continue
		}
		if trust.MaxSensitivity != "" && sensitivityRank[label] == maxRank && label == "internal" {
			redacted := item
			redacted.Fact = redactFact(f)
			kept = append(kept, redacted)
			detail.Redacted++
			continue
		}
		kept = append(kept, item)
	}
	state.AfterTrust = kept
	if snapshotsEnabled(state) {
		detail.Items = candidateSnapshotPtr(contextItemSnapshots(kept))
	}
	return detail, nil
}

func factSensitivity(f domain.TemporalFact) string {
	if f.Metadata == nil {
		return "public"
	}
	if s, ok := f.Metadata[domain.MetaSensitivity].(string); ok && s != "" {
		return strings.ToLower(s)
	}
	return "public"
}

func redactFact(f domain.TemporalFact) domain.TemporalFact {
	out := f.Clone()
	out.Content = ""
	out.EvidenceText = ""
	out.EvidenceRefs = nil
	return out
}

func trustScopes(trust *domain.TrustContext) []domain.Scope {
	if trust == nil || len(trust.Scopes) == 0 {
		return nil
	}
	return trust.Scopes
}

func scopeAllowed(factScope domain.Scope, allowed []domain.Scope) bool {
	for _, s := range allowed {
		if s.RuntimeID == factScope.RuntimeID && s.UserID == factScope.UserID && s.AgentID == factScope.AgentID {
			return true
		}
	}
	return false
}

var (
	_ pipeline.Stage[*read.ReadState]       = (*PolicyFilter)(nil)
	_ pipeline.Conditional[*read.ReadState] = (*PolicyFilter)(nil)
)
