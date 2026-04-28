package graph

// StreamPublisher emits in-flight node events (token / tool_call / tool_result / ...).
//
// Nodes obtain a publisher from ExecutionContext.Publisher and call Emit to
// push deltas. The executor decides where the events ultimately go
// (engine.Host.Publisher, legacy StreamCallback, both) so nodes never see
// those concerns.
//
// Emit is fire-and-forget: implementations must not block the caller and must
// not return errors. Implementations are expected to be safe for concurrent
// use because tool execution may emit from goroutines spawned by the LLM round.
type StreamPublisher interface {
	// Emit pushes one streaming event. Type is a short tag like "token",
	// "tool_call", "tool_result", "delta". Payload SHOULD be a map[string]any
	// so downstream filters can introspect fields without reflection.
	Emit(eventType string, payload any)
}

// StreamPublisherFunc adapts a plain function into a StreamPublisher. Useful
// for tests and for ad-hoc wrapping.
type StreamPublisherFunc func(eventType string, payload any)

// Emit implements StreamPublisher.
func (f StreamPublisherFunc) Emit(eventType string, payload any) {
	if f != nil {
		f(eventType, payload)
	}
}

// noopPublisher discards every event. Returned by NoopPublisher so that node
// authors can use ctx.Publisher unconditionally without nil checks.
type noopPublisher struct{}

// Emit implements StreamPublisher.
func (noopPublisher) Emit(string, any) {}

// NoopPublisher returns a publisher that discards every event. Safe to assign
// to ExecutionContext.Publisher when no observer is registered.
func NoopPublisher() StreamPublisher { return noopPublisher{} }
