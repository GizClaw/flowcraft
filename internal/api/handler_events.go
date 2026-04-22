package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// ReadinessProbe is the small contract /readyz uses; ProjectorManager
// satisfies it. Defined here so handler_events doesn't import projection
// (which would create a cycle once projection imports api types).
type ReadinessProbe interface {
	IsAllReady() bool
	Status() []ProjectorStatusView
}

// ProjectorStatusView is the projector status snapshot the handler exposes
// (mirrors projection.ProjectorStatus by structure, not by import).
type ProjectorStatusView struct {
	Name                string `json:"name"`
	CheckpointSeq       int64  `json:"checkpoint_seq"`
	LatestSeq           int64  `json:"latest_seq"`
	Lag                 int64  `json:"lag"`
	Ready               bool   `json:"ready"`
	ConsecutiveFailures int    `json:"consecutive_failures,omitempty"`
	LastError           string `json:"last_error,omitempty"`
}

// EventsHandler serves the HTTP-pull and readiness endpoints.
type EventsHandler struct {
	log     *eventlog.SQLiteLog
	policy  policy.Policy
	readyer ReadinessProbe
}

// NewEventsHandler wires the handler with its dependencies. readyer may be
// nil during local dev / tests; in that case /readyz returns "ready".
func NewEventsHandler(log *eventlog.SQLiteLog, pol policy.Policy, readyer ReadinessProbe) *EventsHandler {
	return &EventsHandler{log: log, policy: pol, readyer: readyer}
}

// GetEvents handles GET /api/events.
// Query params: partition (required), since (int64, default 0),
// limit (int, default 200, max 1000).
//
// The response body is a JSON object whose `events` array contains the
// canonical envelope encoding (eventlog.MarshalEnvelope), so the bytes for
// any single envelope are byte-equal to what wshub/ssehub would emit.
func (h *EventsHandler) GetEvents(ctx context.Context, r *http.Request) (EventsResponse, error) {
	actor, ok := policy.ActorFrom(ctx)
	if !ok {
		return EventsResponse{}, errUnauthenticated
	}

	partition := r.URL.Query().Get("partition")
	if partition == "" {
		return EventsResponse{}, errBadRequest("partition is required")
	}

	since := int64(0)
	if s := r.URL.Query().Get("since"); s != "" {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return EventsResponse{}, errBadRequest("since must be an integer")
		}
		since = v
	}

	limit := 200
	if s := r.URL.Query().Get("limit"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v <= 0 {
			return EventsResponse{}, errBadRequest("limit must be a positive integer")
		}
		if v > 1000 {
			v = 1000
		}
		limit = v
	}

	dec, err := h.policy.AllowRead(ctx, actor, policy.ReadOptions{Partitions: []string{partition}})
	if err != nil {
		return EventsResponse{}, errInternal(err)
	}
	if !dec.Allow {
		return EventsResponse{}, errForbidden(dec.Reason)
	}

	result, err := h.log.Read(ctx, partition, eventlog.Since(since), limit)
	if err != nil {
		return EventsResponse{}, errInternal(err)
	}
	return EventsResponse{
		Events:    result.Events,
		NextSince: int64(result.NextSince),
		HasMore:   result.HasMore,
	}, nil
}

// GetReadyz handles GET /readyz; returns 503 with not-ready projector names
// when the manager reports any projector behind.
func (h *EventsHandler) GetReadyz(_ context.Context, _ *http.Request) (ReadyzResponse, error) {
	if h.readyer == nil {
		return ReadyzResponse{Status: "ready"}, nil
	}
	if h.readyer.IsAllReady() {
		return ReadyzResponse{Status: "ready"}, nil
	}
	st := h.readyer.Status()
	notReady := make([]string, 0, len(st))
	for _, s := range st {
		if !s.Ready {
			notReady = append(notReady, s.Name)
		}
	}
	return ReadyzResponse{Status: "not_ready", NotReady: notReady}, nil
}

// EventsResponse is the JSON shape returned by GET /api/events. The Events
// slice is encoded through eventlog.MarshalEnvelope (see the custom
// MarshalJSON below).
type EventsResponse struct {
	Events    []eventlog.Envelope
	NextSince int64
	HasMore   bool
}

// MarshalJSON serializes events through the canonical envelope encoder so
// the bytes match wshub/ssehub.
func (e EventsResponse) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"events":[`)
	for i, env := range e.Events {
		if i > 0 {
			buf.WriteByte(',')
		}
		body, err := eventlog.MarshalEnvelope(env)
		if err != nil {
			return nil, err
		}
		buf.Write(body)
	}
	buf.WriteString(`],"next_since":`)
	buf.WriteString(strconv.FormatInt(e.NextSince, 10))
	buf.WriteString(`,"has_more":`)
	if e.HasMore {
		buf.WriteString("true")
	} else {
		buf.WriteString("false")
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// ReadyzResponse is the JSON shape returned by /readyz.
type ReadyzResponse struct {
	Status   string   `json:"status"`
	NotReady []string `json:"not_ready,omitempty"`
}

// ---- error helpers ----

var errUnauthenticated = &APIError{Code: "unauthenticated", Status: 401}
var errForbidden = func(reason string) *APIError { return &APIError{Code: "forbidden", Status: 403, Reason: reason} }

func errBadRequest(msg string) *APIError {
	return &APIError{Code: "bad_request", Status: 400, Message: msg}
}
func errInternal(err error) *APIError {
	return &APIError{Code: "internal_error", Status: 500, Message: err.Error()}
}

// APIError is the JSON-encoded error envelope returned by the handler.
type APIError struct {
	Code    string `json:"code"`
	Status  int    `json:"-"`
	Message string `json:"message,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return e.Code + ": " + e.Message
	}
	return e.Code
}

func (e *APIError) Respond(w http.ResponseWriter) {
	body, _ := json.Marshal(map[string]any{"error": e})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	w.Write(body)
}

// GetEventsHTTP is the HTTP handler for GET /api/events.
func (h *EventsHandler) GetEventsHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	resp, err := h.GetEvents(r.Context(), r)
	if err != nil {
		if ae, ok := err.(*APIError); ok {
			ae.Respond(w)
			return
		}
		errInternal(err).Respond(w)
		return
	}
	body, err := resp.MarshalJSON()
	if err != nil {
		errInternal(err).Respond(w)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// GetReadyzHTTP is the HTTP handler for GET /readyz.
func (h *EventsHandler) GetReadyzHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	resp, err := h.GetReadyz(r.Context(), r)
	if err != nil {
		if ae, ok := err.(*APIError); ok {
			ae.Respond(w)
			return
		}
		errInternal(err).Respond(w)
		return
	}
	body, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	if resp.Status != "ready" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	w.Write(body)
}

// PullAllEvents pages through the event log starting from since; tests use
// this to assert byte-equal envelopes across transports.
func PullAllEvents(ctx context.Context, log *eventlog.SQLiteLog, partition string, since int64) ([]eventlog.Envelope, error) {
	var all []eventlog.Envelope
	current := since
	for {
		result, err := log.Read(ctx, partition, eventlog.Since(current), 1000)
		if err != nil {
			return nil, err
		}
		all = append(all, result.Events...)
		if !result.HasMore {
			break
		}
		current = int64(result.NextSince)
		if len(all) > 100000 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return all, nil
}
