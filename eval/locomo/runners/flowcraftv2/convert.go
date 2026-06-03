package flowcraftv2

import (
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
)

const (
	validFromSourceMetadataKey          = "valid_from_source"
	validFromSourceContentExplicitValue = "content_explicit"
	validFromSourceContentRelativeValue = "content_relative"
	validFromSourceTimeFallbackValue    = "source_time_fallback"
)

func toRecallScope(s runners.Scope) recall.Scope {
	return recall.Scope{
		RuntimeID: s.RuntimeID,
		UserID:    s.UserID,
		AgentID:   s.AgentID,
	}
}

func fromRecallArtifact(h recall.Hit) runners.RecallArtifact {
	if h.Fact.ID == "" && h.Observation.ID != "" {
		artifact := runners.RecallArtifact{
			ID:      h.Observation.ID,
			Content: groundedObservationContent(h),
			Score:   h.Score,
			Kind:    "observation",
		}
		if len(h.Sources) > 0 {
			artifact.Sources = append([]string(nil), h.Sources...)
		}
		for _, ref := range h.Evidence {
			if ref.ID != "" {
				artifact.EvidenceIDs = append(artifact.EvidenceIDs, ref.ID)
			}
		}
		return artifact
	}
	artifact := runners.RecallArtifact{
		ID:      h.Fact.ID,
		Content: groundedHitContent(h),
		Score:   h.Score,
		Kind:    string(h.Fact.Kind),
	}
	if len(h.Sources) > 0 {
		artifact.Sources = append([]string(nil), h.Sources...)
	}
	if h.Fact.ValidFrom != nil && !h.Fact.ValidFrom.IsZero() && rendersEventTime(h.Fact.Metadata) {
		artifact.ValidFrom = h.Fact.ValidFrom.Format("2006-01-02")
	}
	evidence := h.Evidence
	if len(evidence) == 0 {
		evidence = h.Fact.EvidenceRefs
	}
	for _, ref := range evidence {
		if ref.ID != "" {
			artifact.EvidenceIDs = append(artifact.EvidenceIDs, ref.ID)
		}
	}
	if len(h.Fact.Metadata) > 0 {
		artifact.Metadata = make(map[string]any, len(h.Fact.Metadata))
		for k, v := range h.Fact.Metadata {
			artifact.Metadata[k] = v
		}
	}
	return artifact
}

func groundedObservationContent(h recall.Hit) string {
	evidence := h.Evidence
	parts := make([]string, 0, 1+len(evidence))
	appendPart := func(s string) {
		s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
		if s != "" {
			parts = append(parts, s)
		}
	}
	appendPart(h.Observation.Text)
	for _, ref := range evidence {
		appendPart(renderEvidencePart(ref))
	}
	return strings.Join(parts, " | ")
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
		switch validFromRenderKind(f.Metadata) {
		case validFromRenderEventTime:
			parts = append(parts, fmt.Sprintf("[time: %s]", f.ValidFrom.Format("2006-01-02")))
		case validFromRenderObservedAt:
			parts = append(parts, fmt.Sprintf("[observed_at: %s]", f.ValidFrom.Format("2006-01-02")))
		}
	}
	appendPart(f.Content)
	appendPart(f.EvidenceText)
	for _, ref := range evidence {
		appendPart(renderEvidencePart(ref))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(dedupeRenderedParts(parts), " | evidence: ")
}

func rendersEventTime(meta map[string]any) bool {
	return validFromRenderKind(meta) == validFromRenderEventTime
}

type validFromRenderMode int

const (
	validFromRenderEventTime validFromRenderMode = iota
	validFromRenderObservedAt
)

func validFromRenderKind(meta map[string]any) validFromRenderMode {
	raw, ok := meta[validFromSourceMetadataKey]
	if !ok {
		return validFromRenderEventTime
	}
	source, ok := raw.(string)
	if !ok {
		return validFromRenderEventTime
	}
	switch strings.TrimSpace(source) {
	case validFromSourceContentExplicitValue, validFromSourceContentRelativeValue:
		return validFromRenderEventTime
	case validFromSourceTimeFallbackValue:
		return validFromRenderObservedAt
	default:
		return validFromRenderEventTime
	}
}

func renderEvidencePart(ref recall.EvidenceRef) string {
	text := strings.TrimSpace(strings.ReplaceAll(ref.Text, "\n", " "))
	if text == "" {
		return ""
	}
	if ref.Timestamp.IsZero() {
		return text
	}
	return fmt.Sprintf("[source_time: %s] %s", evidenceSourceTimeLabel(ref), text)
}

func evidenceSourceTimeLabel(ref recall.EvidenceRef) string {
	ts := ref.Timestamp
	if ts.Hour() == 0 && ts.Minute() == 0 && ts.Second() == 0 && ts.Nanosecond() == 0 {
		return ts.Format("2006-01-02")
	}
	return ts.Format("2006-01-02 15:04")
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

func fromRecallArtifacts(hits []recall.Hit) []runners.RecallArtifact {
	if len(hits) == 0 {
		return nil
	}
	out := make([]runners.RecallArtifact, len(hits))
	for i, h := range hits {
		out[i] = fromRecallArtifact(h)
	}
	return out
}

func renderSourceTurnText(text string, images []runners.RawImage) string {
	text = strings.TrimSpace(text)
	var b strings.Builder
	b.WriteString(text)
	visual := runners.RenderRawImageMetadata(images)
	if visual != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(visual)
	}
	return strings.TrimSpace(b.String())
}

func fromRecallStageAudit(a diagnostics.RecallStageAudit) runners.RecallStageAudit {
	out := runners.RecallStageAudit{Stages: make([]runners.RecallStageSnapshot, 0, len(a.Stages))}
	for _, st := range a.Stages {
		out.Stages = append(out.Stages, runners.RecallStageSnapshot{
			Stage:             st.Stage,
			Source:            st.Source,
			Status:            st.Status,
			Query:             fromRecallQueryIntent(st.Query),
			ActivatedLenses:   fromRecallActivatedLenses(st.ActivatedLenses),
			TaskIntents:       append([]string(nil), st.TaskIntents...),
			TotalBudget:       st.TotalBudget,
			Suggested:         st.Suggested,
			SuggestedByTask:   cloneIntMap(st.SuggestedByTask),
			SuggestedFactIDs:  append([]string(nil), st.SuggestedFactIDs...),
			Added:             st.Added,
			AddedFactIDs:      append([]string(nil), st.AddedFactIDs...),
			ScannedLinks:      st.ScannedLinks,
			AddedFacts:        st.AddedFacts,
			AddedEvidenceRefs: st.AddedEvidenceRefs,
			CoverageBundles:   fromRecallCoverageBundles(st.CoverageBundles),
			Candidates:        fromRecallAuditCandidates(st.Candidates),
			PackTrace:         fromRecallAuditCandidates(st.PackTrace),
		})
	}
	return out
}

func fromRecallQueryIntent(in *diagnostics.RecallQueryIntent) *runners.RecallQueryIntent {
	if in == nil {
		return nil
	}
	return &runners.RecallQueryIntent{
		QueryLen:                      in.QueryLen,
		Entities:                      append([]string(nil), in.Entities...),
		Kinds:                         append([]string(nil), in.Kinds...),
		Subject:                       in.Subject,
		Predicate:                     in.Predicate,
		Object:                        in.Object,
		HasTimeRange:                  in.HasTimeRange,
		HasExplicitDate:               in.HasExplicitDate,
		HasRelativeTemporalExpression: in.HasRelativeTemporalExpression,
		TokenCount:                    in.TokenCount,
		NumericCount:                  in.NumericCount,
		QuotedCount:                   in.QuotedCount,
		ProperCount:                   in.ProperCount,
		Strategy:                      in.Strategy,
		Confidence:                    in.Confidence,
		Alternates:                    fromRecallIntentRouteCandidates(in.Alternates),
		Signals:                       append([]string(nil), in.Signals...),
		FallbackReason:                in.FallbackReason,
	}
}

func fromRecallIntentRouteCandidates(in []diagnostics.RecallIntentRouteCandidate) []runners.RecallIntentRouteCandidate {
	if len(in) == 0 {
		return nil
	}
	out := make([]runners.RecallIntentRouteCandidate, 0, len(in))
	for _, candidate := range in {
		out = append(out, runners.RecallIntentRouteCandidate{
			Strategy:   candidate.Strategy,
			Confidence: candidate.Confidence,
		})
	}
	return out
}

func fromRecallActivatedLenses(in []diagnostics.RecallActivatedLens) []runners.RecallActivatedLens {
	if len(in) == 0 {
		return nil
	}
	out := make([]runners.RecallActivatedLens, 0, len(in))
	for _, lens := range in {
		out = append(out, runners.RecallActivatedLens{
			Lens:        lens.Lens,
			Weight:      lens.Weight,
			Budget:      lens.Budget,
			ActivatedBy: lens.ActivatedBy,
		})
	}
	return out
}

func fromRecallCoverageBundles(in []diagnostics.RecallCoverageBundle) []runners.RecallCoverageBundle {
	if len(in) == 0 {
		return nil
	}
	out := make([]runners.RecallCoverageBundle, 0, len(in))
	for _, bundle := range in {
		out = append(out, runners.RecallCoverageBundle{
			SeedFactID:      bundle.SeedFactID,
			RescuedFactIDs:  append([]string(nil), bundle.RescuedFactIDs...),
			ReplacedFactIDs: append([]string(nil), bundle.ReplacedFactIDs...),
			Reason:          bundle.Reason,
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
			FactID:           c.FactID,
			Source:           c.Source,
			Rank:             c.Rank,
			Score:            c.Score,
			EvidenceIDs:      append([]string(nil), c.EvidenceIDs...),
			Sources:          append([]string(nil), c.Sources...),
			RankOutputRank:   c.RankOutputRank,
			ContextPackRank:  c.ContextPackRank,
			PrimarySource:    c.PrimarySource,
			ProjectionRoutes: append([]string(nil), c.ProjectionRoutes...),
			DroppedReason:    c.DroppedReason,
		})
	}
	return out
}

func cloneIntMap(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
