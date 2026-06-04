package port

// ScrubAsyncSemanticJobPII clears enqueue-time context from a
// terminal async semantic job so completed and dead-letter rows do
// not retain RecentMessages, ExistingFactHints, EvidenceWindowRefs,
// or SourceEvidenceSpans.
func ScrubAsyncSemanticJobPII(job *AsyncSemanticJob) {
	if job == nil {
		return
	}
	job.SourceEvidenceSpans = nil
	job.RecentMessages = nil
	job.ExistingFactHints = nil
	job.EvidenceWindowRefs = nil
}
