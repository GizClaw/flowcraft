package session

import "context"

// streamPolicyKey is the context key for a turn's inherited stream
// policy.
type streamPolicyKey struct{}

// StreamPolicy describes how a turn's stream sinks are inherited by
// sessions started within the turn's execution (for example delegated
// subagent runs). Session starts stamp their attached sinks so nested
// runs inherit the caller's stream without any explicit wiring.
//
// Inheritance shares the same Sink instance across sessions: a delegated
// turn attaches the caller's sinks too, so a sink must be safe for
// concurrent OnDelta calls. The delegation boundary downgrades inherited
// specs to observers (no authority, no explicit acks), because the
// subagent turn is never handed to the sink.
type StreamPolicy struct {
	// Sinks are attached to every nested session started inside the
	// turn when Inheritable is true. Delegation attaches them as
	// observers.
	Sinks []SinkSpec
	// Inheritable reports whether nested sessions should inherit Sinks.
	// Session starts stamp the policy with Inheritable=true; callers may
	// stamp false to stop inheritance at a boundary.
	Inheritable bool
}

// WithStreamPolicy returns a context that carries policy to nested
// sessions started within the turn's execution.
func WithStreamPolicy(ctx context.Context, policy StreamPolicy) context.Context {
	return context.WithValue(ctx, streamPolicyKey{}, policy)
}

// StreamPolicyFromContext returns the nearest stream policy carried by
// ctx, if any.
func StreamPolicyFromContext(ctx context.Context) (StreamPolicy, bool) {
	policy, ok := ctx.Value(streamPolicyKey{}).(StreamPolicy)
	return policy, ok
}
