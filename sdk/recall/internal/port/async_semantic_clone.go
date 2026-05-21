package port

import "github.com/GizClaw/flowcraft/sdk/recall/internal/domain"

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
	if len(job.RecentMessages) > 0 {
		out.RecentMessages = append([]domain.Message(nil), job.RecentMessages...)
	}
	if len(job.ExistingFactsAnchor) > 0 {
		out.ExistingFactsAnchor = make([]domain.TemporalFact, len(job.ExistingFactsAnchor))
		for i, f := range job.ExistingFactsAnchor {
			out.ExistingFactsAnchor[i] = f.Clone()
		}
	}
	return out
}
