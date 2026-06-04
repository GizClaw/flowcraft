package port

import "github.com/GizClaw/flowcraft/memory/recall/internal/domain"

// CloneAsyncSemanticJob returns a defensive copy of job suitable for
// durable enqueue. Slice fields alias caller memory after Save returns
// unless cloned at the outbox boundary.
func CloneAsyncSemanticJob(job AsyncSemanticJob) AsyncSemanticJob {
	out := job
	if len(job.EpisodeFactIDs) > 0 {
		out.EpisodeFactIDs = append([]string(nil), job.EpisodeFactIDs...)
	}
	if len(job.TurnsSnapshot) > 0 {
		out.TurnsSnapshot = append([]domain.TurnContext(nil), job.TurnsSnapshot...)
	}
	if len(job.SourceEvidenceSpans) > 0 {
		out.SourceEvidenceSpans = append([]domain.SourceEvidenceSpan(nil), job.SourceEvidenceSpans...)
	}
	if len(job.RecentMessages) > 0 {
		out.RecentMessages = append([]domain.Message(nil), job.RecentMessages...)
	}
	if len(job.ExistingFactHints) > 0 {
		out.ExistingFactHints = make([]domain.TemporalFact, len(job.ExistingFactHints))
		for i, f := range job.ExistingFactHints {
			out.ExistingFactHints[i] = f.Clone()
		}
	}
	if len(job.EvidenceWindowRefs) > 0 {
		out.EvidenceWindowRefs = append([]domain.EvidenceWindowRef(nil), job.EvidenceWindowRefs...)
	}
	return out
}
