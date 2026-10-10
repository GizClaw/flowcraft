package pool

import "context"

// A reason is an opaque string an application tags on ctx to say what
// asked for a member: a settings save, a plugin change, a workspace
// switch, a retry after a drain. The pool does not interpret it — it
// travels to Spec.Open and lands in the pool's own log lines, which is
// what lets a rebuild storm be answered from the log alone: who asked
// for each rebuild, and which ones were deferred behind live work.
type reasonKey struct{}

// ReasonRetryAfterDrain is the reason the pool tags on the context of a
// deferred replacement's assembly: the member being replaced was
// invalidated and drained first (see ScheduleReplacement).
const ReasonRetryAfterDrain = "retry_after_drain"

// WithReason tags ctx so the next assembly — and any invalidation on
// the way to it — reports reason instead of "unknown". An empty reason
// leaves ctx alone; a nil ctx becomes context.Background().
func WithReason(ctx context.Context, reason string) context.Context {
	if reason == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, reasonKey{}, reason)
}

// ReasonFrom returns the reason tagged on ctx, or "unknown" — the value
// an untagged call reports, matching what a caller that never named a
// reason should look like in the log.
func ReasonFrom(ctx context.Context) string {
	return reasonFrom(ctx)
}

// reasonFrom is ReasonFrom's internal form.
func reasonFrom(ctx context.Context) string {
	if ctx == nil {
		return "unknown"
	}
	reason, _ := ctx.Value(reasonKey{}).(string)
	if reason == "" {
		return "unknown"
	}
	return reason
}
