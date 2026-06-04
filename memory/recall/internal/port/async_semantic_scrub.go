package port

// ScrubAsyncSemanticJobPII clears enqueue-time snapshots from a
// terminal async semantic job so completed and dead-letter rows do
// not retain TurnsSnapshot, RecentMessages, ExistingFactHints, or
// EvidenceWindowRefs / SourceEvidenceSpans.
func ScrubAsyncSemanticJobPII(job *AsyncSemanticJob) {
	if job == nil {
		return
	}
	job.TurnsSnapshot = nil
	job.SourceEvidenceSpans = nil
	job.RecentMessages = nil
	job.ExistingFactHints = nil
	job.EvidenceWindowRefs = nil
}
