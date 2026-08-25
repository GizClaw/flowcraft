package delegation

import (
	"context"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/runtime/session"
)

// Metadata keys injected service-side into an async delegation's
// Request.Metadata so operational views (kanban cards, delegation
// status) surface run lineage without any per-view plumbing. They are
// set by the service, not accepted from user-supplied delegate
// arguments; callers must not treat them as authoritative input.
const (
	// ParentRunMetadataKey carries the parent (caller) run id.
	ParentRunMetadataKey = "delegation.parent_run"
	// CallIDMetadataKey carries the delegate tool call id that
	// started the delegation.
	CallIDMetadataKey = "delegation.call_id"
)

// StreamTarget is a serializable description of where an async
// subagent's stream envelopes should be delivered. It replaces the
// live (non-serializable) sink across the queue boundary: the caller's
// runtime knows how to describe its sinks as targets, and the worker's
// runtime resolves them back into live sinks through
// [StreamTargetResolver].
type StreamTarget struct {
	// Kind is the target vocabulary understood by the resolver
	// (e.g. "conversation", "bus"). Resolvers MUST reject unknown
	// kinds — targets originate from persisted backend records and
	// must never drive arbitrary sink construction.
	Kind string `json:"kind"`
	// ID is the resolver-specific destination identifier.
	ID string `json:"id"`
}

// StreamPolicySnapshot is a serializable projection of the caller's
// stream attachment policy, carried alongside a [StreamTarget] so the
// worker can materialize the attachment with the same tuning. It is
// populated from the caller's first sink spec; authority/ack fields
// are informational and the worker always downgrades inherited
// attachments to observers (see inheritedSinkSpecs).
type StreamPolicySnapshot struct {
	Visibility      session.Visibility `json:"visibility,omitempty"`
	Authority       session.Authority  `json:"authority,omitempty"`
	AckMode         session.AckMode    `json:"ack_mode,omitempty"`
	MaxUnacked      int                `json:"max_unacked,omitempty"`
	QueueSize       int                `json:"queue_size,omitempty"`
	DeliveryTimeout int64              `json:"delivery_timeout_ms,omitempty"`
}

// StreamRef is the queue-crossing reference to an async delegation's
// stream attachment. In-process backends use Ref (an escrow key that
// the service resolves to the caller's live sinks); cross-process
// backends use Target with a runtime-provided resolver.
type StreamRef struct {
	// Ref is the service-side escrow key. Empty when no in-process
	// escrow exists.
	Ref string `json:"ref,omitempty"`
	// Policy snapshots the caller's attachment tuning.
	Policy StreamPolicySnapshot `json:"policy,omitempty"`
	// Target describes the delivery destination for resolver-based
	// materialization. Empty when the escrow path is used.
	Target *StreamTarget `json:"target,omitempty"`
}

// StreamTargetResolver materializes a live stream sink for a
// serializable [StreamTarget] on the worker side. Resolvers MUST
// whitelist target kinds and never construct sinks from free-form
// persisted data.
type StreamTargetResolver func(ctx context.Context, target StreamTarget) (agent.StreamSink, error)

// StreamTargetExporter describes a caller-side live sink as a
// serializable [StreamTarget] at async submit time, so cross-process
// backends can re-materialize the same destination worker-side through
// a [StreamTargetResolver]. It returns ok=false for sinks it cannot
// describe (e.g. plain closures with no durable destination).
type StreamTargetExporter func(spec session.SinkSpec) (StreamTarget, bool)

// RunIDNotifier is an optional AsyncBackend capability: it lets a
// claimed async work item record the subagent run id as soon as the
// run starts, so operational views can correlate a delegation id with
// its run before the terminal response arrives.
type RunIDNotifier interface {
	// NoteRunID records runID for the work item identified by its
	// stream escrow ref. Unknown refs are no-ops; errors are
	// best-effort (recorded, never fail the run).
	NoteRunID(ctx context.Context, ref, runID string) error
}
