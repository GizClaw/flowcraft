package flowcraftv2

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
)

func toRecallScope(s runners.Scope) recall.Scope {
	return recall.Scope{
		RuntimeID: s.RuntimeID,
		UserID:    s.UserID,
		AgentID:   s.AgentID,
	}
}

func fromRecallHit(h recall.Hit) runners.Hit {
	hit := runners.Hit{
		ID:      h.Fact.ID,
		Content: groundedHitContent(h),
		Score:   h.Score,
		Kind:    string(h.Fact.Kind),
	}
	if len(h.Sources) > 0 {
		hit.Sources = append([]string(nil), h.Sources...)
	}
	if h.Fact.ValidFrom != nil && !h.Fact.ValidFrom.IsZero() {
		hit.ValidFrom = h.Fact.ValidFrom.Format("2006-01-02")
	}
	evidence := h.Evidence
	if len(evidence) == 0 {
		evidence = h.Fact.EvidenceRefs
	}
	for _, ref := range evidence {
		if ref.ID != "" {
			hit.EvidenceIDs = append(hit.EvidenceIDs, ref.ID)
		}
	}
	if len(h.Fact.Metadata) > 0 {
		hit.Metadata = make(map[string]any, len(h.Fact.Metadata))
		for k, v := range h.Fact.Metadata {
			hit.Metadata[k] = v
		}
	}
	return hit
}

func groundedHitContent(h recall.Hit) string {
	f := h.Fact
	evidence := h.Evidence
	if len(evidence) == 0 {
		evidence = f.EvidenceRefs
	}
	parts := make([]string, 0, 3+len(evidence))
	appendPart := func(s string) {
		s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
		if s != "" {
			parts = append(parts, s)
		}
	}
	// This is LoCoMo answer-context shaping, not an SDK contract: the
	// benchmark answer prompt expects temporal facts to expose the resolved
	// date inline so the answer LLM does not recompute relative expressions.
	if f.ValidFrom != nil && !f.ValidFrom.IsZero() {
		parts = append(parts, fmt.Sprintf("[time: %s]", f.ValidFrom.Format("2006-01-02")))
	}
	appendPart(f.Content)
	appendPart(f.EvidenceText)
	for _, ref := range evidence {
		appendPart(ref.Text)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(dedupeRenderedParts(parts), " | evidence: ")
}

func dedupeRenderedParts(parts []string) []string {
	if len(parts) < 2 {
		return parts
	}
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		key := strings.ToLower(strings.Join(strings.Fields(part), " "))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, part)
	}
	return out
}

func fromRecallHits(hits []recall.Hit) []runners.Hit {
	if len(hits) == 0 {
		return nil
	}
	out := make([]runners.Hit, len(hits))
	for i, h := range hits {
		out[i] = fromRecallHit(h)
	}
	return out
}

func fromRecallStageAudit(a diagnostics.RecallStageAudit) runners.RecallStageAudit {
	out := runners.RecallStageAudit{Stages: make([]runners.RecallStageSnapshot, 0, len(a.Stages))}
	for _, st := range a.Stages {
		out.Stages = append(out.Stages, runners.RecallStageSnapshot{
			Stage:      st.Stage,
			Source:     st.Source,
			Status:     st.Status,
			Candidates: fromRecallAuditCandidates(st.Candidates),
		})
	}
	return out
}

func fromRecallAuditCandidates(in []diagnostics.RecallCandidateSnapshot) []runners.RecallCandidateSnapshot {
	if len(in) == 0 {
		return nil
	}
	out := make([]runners.RecallCandidateSnapshot, 0, len(in))
	for _, c := range in {
		out = append(out, runners.RecallCandidateSnapshot{
			FactID:      c.FactID,
			Source:      c.Source,
			Rank:        c.Rank,
			Score:       c.Score,
			EvidenceIDs: append([]string(nil), c.EvidenceIDs...),
			Sources:     append([]string(nil), c.Sources...),
		})
	}
	return out
}
