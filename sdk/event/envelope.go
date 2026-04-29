package event

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/xid"
	"go.opentelemetry.io/otel/trace"
)

// Envelope is the cross-process-friendly carrier for a single event.
//
// Every field serialises cleanly to JSON. Payload is stored as
// json.RawMessage so:
//
//  1. in-memory and remote bus implementations behave identically (bytes in,
//     bytes out — no any-typed surprises);
//  2. server-side persistence and SSE forwarding can avoid an extra round
//     of marshal/unmarshal;
//  3. consumers decide when to decode (and into what concrete type).
//
// Use NewEnvelope / MustEnvelope to construct envelopes; use Decode to
// extract the payload into a typed Go value.
type Envelope struct {
	ID      string            `json:"id"`
	Subject Subject           `json:"subject"`
	Time    time.Time         `json:"time"`
	Source  string            `json:"source,omitempty"`
	TraceID string            `json:"trace_id,omitempty"`
	SpanID  string            `json:"span_id,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Payload json.RawMessage   `json:"payload,omitempty"`
}

// Well-known header keys. Consumers may add arbitrary headers; these
// constants exist to prevent key drift across producers.
const (
	HeaderRunID   = "run_id"
	HeaderNodeID  = "node_id"
	HeaderActorID = "actor_id"
	HeaderGraphID = "graph_id"
	HeaderTenant  = "tenant"

	// HeaderKanbanScopeID identifies the sdk/kanban Board scope
	// (Board.ScopeID) that produced an envelope. Distinct from
	// HeaderRunID — kanban events do not happen inside an engine run
	// — so consumers that want to fan-in by board need a dedicated
	// dimension. Stored as a typed header (mirroring HeaderRunID /
	// HeaderGraphID) rather than crammed into Envelope.Source so it
	// composes with the existing well-known-header convention.
	HeaderKanbanScopeID = "kanban_scope_id"
)

// NewEnvelope constructs an Envelope with ID and Time populated.
//
// Behaviour:
//   - if subject is empty, returns ErrInvalidSubject (Subject.Validate is
//     called for early rejection of malformed routing keys);
//   - if payload is nil, Payload stays nil (no "null" bytes);
//   - if payload is already a json.RawMessage, it is reused verbatim — no
//     re-encoding;
//   - otherwise, payload is JSON-encoded;
//   - if ctx carries an OTel span, TraceID / SpanID are filled from it
//     so downstream subscribers can correlate envelopes back to the
//     producing trace without separate plumbing.
func NewEnvelope(ctx context.Context, subject Subject, payload any) (Envelope, error) {
	if err := subject.Validate(); err != nil {
		return Envelope{}, err
	}

	env := Envelope{
		ID:      xid.New().String(),
		Subject: subject,
		Time:    time.Now(),
	}

	switch p := payload.(type) {
	case nil:
		// leave Payload nil
	case json.RawMessage:
		if p != nil {
			env.Payload = p
		}
	case []byte:
		// Treat raw bytes as already-encoded JSON to allow callers to opt
		// out of re-encoding; we do not validate the bytes here.
		if p != nil {
			env.Payload = json.RawMessage(p)
		}
	default:
		buf, err := json.Marshal(payload)
		if err != nil {
			return Envelope{}, fmt.Errorf("event: marshal payload for subject %q: %w", subject, err)
		}
		env.Payload = buf
	}

	if sc := trace.SpanFromContext(ctx).SpanContext(); sc.IsValid() {
		env.TraceID = sc.TraceID().String()
		env.SpanID = sc.SpanID().String()
	}

	return env, nil
}

// MustEnvelope is the panic variant of NewEnvelope, intended for static
// initialisation paths where a marshalling failure indicates a programming
// bug rather than a runtime condition.
func MustEnvelope(ctx context.Context, subject Subject, payload any) Envelope {
	env, err := NewEnvelope(ctx, subject, payload)
	if err != nil {
		panic(fmt.Errorf("event.MustEnvelope: %w", err))
	}
	return env
}

// Decode unmarshals Payload into out. Returns nil (no-op) when Payload is
// empty so that callers can ignore decode for header-only events.
func (e Envelope) Decode(out any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(e.Payload, out); err != nil {
		return fmt.Errorf("event: decode payload for subject %q: %w", e.Subject, err)
	}
	return nil
}

// SetHeader sets a header value, allocating Headers lazily.
func (e *Envelope) SetHeader(key, value string) {
	if e.Headers == nil {
		e.Headers = make(map[string]string, 4)
	}
	e.Headers[key] = value
}

// Header returns the header value or "" when absent.
func (e Envelope) Header(key string) string {
	if e.Headers == nil {
		return ""
	}
	return e.Headers[key]
}

// SetRunID is a typed shorthand for SetHeader(HeaderRunID, id).
func (e *Envelope) SetRunID(id string) { e.SetHeader(HeaderRunID, id) }

// RunID returns the value of the well-known run_id header.
func (e Envelope) RunID() string { return e.Header(HeaderRunID) }

// SetNodeID is a typed shorthand for SetHeader(HeaderNodeID, id).
func (e *Envelope) SetNodeID(id string) { e.SetHeader(HeaderNodeID, id) }

// NodeID returns the value of the well-known node_id header.
func (e Envelope) NodeID() string { return e.Header(HeaderNodeID) }

// SetActorID is a typed shorthand for SetHeader(HeaderActorID, id).
func (e *Envelope) SetActorID(id string) { e.SetHeader(HeaderActorID, id) }

// ActorID returns the value of the well-known actor_id header.
func (e Envelope) ActorID() string { return e.Header(HeaderActorID) }

// SetGraphID is a typed shorthand for SetHeader(HeaderGraphID, id).
func (e *Envelope) SetGraphID(id string) { e.SetHeader(HeaderGraphID, id) }

// GraphID returns the value of the well-known graph_id header.
func (e Envelope) GraphID() string { return e.Header(HeaderGraphID) }

// SetTenant is a typed shorthand for SetHeader(HeaderTenant, id).
func (e *Envelope) SetTenant(id string) { e.SetHeader(HeaderTenant, id) }

// Tenant returns the value of the well-known tenant header.
func (e Envelope) Tenant() string { return e.Header(HeaderTenant) }

// SetKanbanScopeID is a typed shorthand for
// SetHeader(HeaderKanbanScopeID, id).
func (e *Envelope) SetKanbanScopeID(id string) { e.SetHeader(HeaderKanbanScopeID, id) }

// KanbanScopeID returns the value of the well-known kanban_scope_id header.
func (e Envelope) KanbanScopeID() string { return e.Header(HeaderKanbanScopeID) }
