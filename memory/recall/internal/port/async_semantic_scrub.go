package port

// ScrubAsyncSemanticJobPII clears enqueue-time snapshots from a
// terminal async semantic job so completed and dead-letter rows do
// not retain TurnsSnapshot, RecentMessages, or ExistingFactsAnchor.
func ScrubAsyncSemanticJobPII(job *AsyncSemanticJob) {
	if job == nil {
		return
	}
	job.TurnsSnapshot = nil
	job.RecentMessages = nil
	job.ExistingFactsAnchor = nil
}
