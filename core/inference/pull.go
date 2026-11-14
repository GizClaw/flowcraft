package inference

import "context"

// Pull is a pull-based sequence of values. Next returns the next value, and
// returns [io.EOF] once the sequence ended cleanly; any other error is terminal.
// Close releases whatever the sequence holds.
//
// It is the single termination rule the whole stack follows: a host reading
// [Stream], the pipeline reading a provider's wire stream, and a driver adapter
// reading its SDK's cursor all end on io.EOF. One rule is why the pipeline needs no
// translation layer between the wire and the host face, and why a generic
// consumer — a tee, a rate limiter, a timeout wrapper — works on either.
//
// Pull lives here rather than in a utility package because it is a contract, not a
// helper: the rule it states is the rule every layer must obey, and nothing outside
// this package imports it.
type Pull[T any] interface {
	// Next returns the next value, or [io.EOF] when the sequence has ended.
	Next(ctx context.Context) (T, error)
}
