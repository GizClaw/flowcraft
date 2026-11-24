package a2a

import (
	"context"
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/goccy/go-json"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// RecordCall is the kind one proxied round trip is written under.
//
// A proxied task has no local model call and no local tool execution: the peer owns both, so the
// record kinds the graph writes (`llm.call`, `graph.effect`) do not exist on this side. What this
// side knows is the round trip itself — how long the peer took, which of its tasks answered, and
// how it ended — and without it a deployment of proxies has a ledger with nothing in it but state
// transitions.
const RecordCall = "a2a.call"

// CallRecord is one proxied round trip as the event log keeps it.
//
// It carries no answer text and no token counts: the peer measures its own usage, and a proxy that
// invented numbers would be reporting accounting nobody took. What is here is what the calling side
// can measure — latency, the peer's identity and the outcome — so a slow peer and a failing peer
// are distinguishable in the log alone.
type CallRecord struct {
	// PeerTaskID and ContextID name the peer's task and conversation. They are the ids a
	// reader quotes when asking the peer what it did with the message.
	PeerTaskID string `json:"peer_task_id,omitempty"`
	ContextID  string `json:"context_id,omitempty"`
	// State is the lifecycle the peer last reported in this round trip.
	State agent.TaskState `json:"state,omitempty"`
	// LatencyMs is how long the round trip took, as the calling side measured it.
	LatencyMs int64 `json:"latency_ms,omitempty"`
	// ErrorKind classifies a round trip that failed; empty for a completed one.
	ErrorKind string `json:"error_kind,omitempty"`
}

// Error kinds a proxied call is classified with.
const (
	// callCanceled means the caller stopped the turn.
	callCanceled = "canceled"
	// callTimeout means the turn's deadline ended the round trip.
	callTimeout = "timeout"
	// callPeerError is anything else the peer or the transport reported.
	callPeerError = "peer_error"
)

// callErrorKind maps one failed round trip onto the record's vocabulary.
func callErrorKind(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return callCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return callTimeout
	default:
		return callPeerError
	}
}

// callEvents renders the round trip as the one fact the log keeps about it.
//
// It is written in the same save as the peer's identity, so a reader never sees a peer task the
// log has no round trip for — and so a proxy pays one commit per call rather than two.
func callEvents(remembered remote, started time.Time, failure error) ([]agent.EventRecord, error) {
	payload, err := json.Marshal(CallRecord{
		PeerTaskID: remembered.TaskID,
		ContextID:  remembered.ContextID,
		State:      remembered.State,
		LatencyMs:  time.Since(started).Milliseconds(),
		ErrorKind:  callErrorKind(failure),
	})
	if err != nil {
		return nil, err
	}
	return []agent.EventRecord{{Kind: RecordCall, Payload: payload, At: started}}, nil
}

// finishSendSpan closes one round trip with the classification, never the message.
//
// The peer's error text can carry its own ids, paths and provider wording, so it goes to the log
// record the driver already writes; the span carries the same vocabulary the record does, which is
// what lets a query over traces and a query over the log select the same incident.
func finishSendSpan(span trace.Span, state agent.TaskState, err error) {
	if state != "" {
		span.SetAttributes(attribute.String(telemetry.AttrA2AState, string(state)))
	}
	if err != nil {
		kind := callErrorKind(err)
		span.SetAttributes(attribute.String(telemetry.AttrErrorKind, kind))
		span.SetStatus(codes.Error, kind)
		return
	}
	span.SetStatus(codes.Ok, "")
}
