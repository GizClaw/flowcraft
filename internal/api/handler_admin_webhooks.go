package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"

	"github.com/GizClaw/flowcraft/internal/eventlog"
)

type webhookDeliveryDTO struct {
	DeliveryID     string `json:"delivery_id"`
	EndpointID     string `json:"endpoint_id"`
	SourceEventSeq int64  `json:"source_event_seq,omitempty"`
	Status         string `json:"status"`
	Attempts       int32  `json:"attempts"`
	LastStatusCode int32  `json:"last_status_code,omitempty"`
	LastAttemptAt  string `json:"last_attempt_at,omitempty"`
	URL            string `json:"url,omitempty"`
	Method         string `json:"method,omitempty"`
	LastError      string `json:"last_error,omitempty"`
}

// handleAdminWebhookDeliveries answers GET /api/admin/webhooks/deliveries.
// Aggregates webhook.outbound.* envelopes from the event log into one row
// per delivery_id. Filters: endpoint_id, status, limit (default 50, max 500).
func (s *Server) handleAdminWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	endpointFilter := q.Get("endpoint_id")
	statusFilter := q.Get("status")

	deliveries, err := s.aggregateWebhookDeliveries(r.Context(), endpointFilter, statusFilter, limit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deliveries)
}

// handleAdminWebhookReplay answers POST
// /api/admin/webhooks/deliveries/{id}/replay. The actual rescheduling is
// handed off to the WebhookReplayer (defined as an interface so the API
// package doesn't import the senders package directly).
func (s *Server) handleAdminWebhookReplay(w http.ResponseWriter, r *http.Request) {
	if !s.requireSuperAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{
			"code":    "bad_request",
			"message": "delivery id required",
		}})
		return
	}
	if s.deps.WebhookReplayer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{
			"code":    "not_available",
			"message": "webhook replayer not configured",
		}})
		return
	}
	if err := s.deps.WebhookReplayer.ReplayDelivery(r.Context(), id); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]string{
			"code":    "replay_failed",
			"message": err.Error(),
		}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "scheduled", "delivery_id": id})
}

// aggregateWebhookDeliveries scans every webhook.outbound.* envelope in the
// event log (oldest → newest) and folds them into one DTO per delivery_id.
// We page through the log in chunks of 1000 so a long-running deployment
// doesn't materialise the entire history in memory.
func (s *Server) aggregateWebhookDeliveries(ctx context.Context, endpointFilter, statusFilter string, limit int) ([]webhookDeliveryDTO, error) {
	state := map[string]*webhookDeliveryDTO{}

	var since int64
	for {
		res, err := s.deps.EventLog.ReadAll(ctx, eventlog.Since(since), 1000)
		if err != nil {
			return nil, err
		}
		for _, env := range res.Events {
			switch env.Type {
			case eventlog.EventTypeWebhookOutboundQueued:
				var p eventlog.WebhookOutboundQueuedPayload
				if json.Unmarshal(env.Payload, &p) != nil {
					continue
				}
				d := lookupOrCreate(state, p.DeliveryID)
				d.EndpointID = p.EndpointID
				d.SourceEventSeq = p.SourceEventSeq
				d.URL = p.URL
				d.Method = p.Method
				if d.Status == "" {
					d.Status = "pending"
				}
			case eventlog.EventTypeWebhookOutboundScheduled:
				var p eventlog.WebhookOutboundScheduledPayload
				if json.Unmarshal(env.Payload, &p) != nil {
					continue
				}
				d := lookupOrCreate(state, p.DeliveryID)
				d.EndpointID = p.EndpointID
				if p.Attempt > d.Attempts {
					d.Attempts = p.Attempt
				}
				d.LastAttemptAt = p.NotBefore
				d.Status = "pending"
			case eventlog.EventTypeWebhookOutboundSent:
				var p eventlog.WebhookOutboundSentPayload
				if json.Unmarshal(env.Payload, &p) != nil {
					continue
				}
				d := lookupOrCreate(state, p.DeliveryID)
				d.EndpointID = p.EndpointID
				d.Attempts = p.Attempt
				d.LastStatusCode = p.StatusCode
				d.LastAttemptAt = env.Ts
				d.Status = "sent"
				d.LastError = ""
			case eventlog.EventTypeWebhookOutboundAttemptFailed:
				var p eventlog.WebhookOutboundAttemptFailedPayload
				if json.Unmarshal(env.Payload, &p) != nil {
					continue
				}
				d := lookupOrCreate(state, p.DeliveryID)
				d.EndpointID = p.EndpointID
				d.Attempts = p.Attempt
				d.LastStatusCode = p.StatusCode
				d.LastAttemptAt = env.Ts
				if d.Status != "exhausted" {
					d.Status = "failed"
				}
				d.LastError = p.ErrorClass + ": " + p.ErrorMessage
			case eventlog.EventTypeWebhookOutboundExhausted:
				var p eventlog.WebhookOutboundExhaustedPayload
				if json.Unmarshal(env.Payload, &p) != nil {
					continue
				}
				d := lookupOrCreate(state, p.DeliveryID)
				d.EndpointID = p.EndpointID
				d.Attempts = p.TotalAttempts
				d.Status = "exhausted"
				d.LastError = p.FinalError
				d.LastAttemptAt = env.Ts
			}
		}
		if !res.HasMore {
			break
		}
		since = int64(res.NextSince)
	}

	out := make([]webhookDeliveryDTO, 0, len(state))
	for _, d := range state {
		if endpointFilter != "" && d.EndpointID != endpointFilter {
			continue
		}
		if statusFilter != "" && d.Status != statusFilter {
			continue
		}
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastAttemptAt > out[j].LastAttemptAt })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func lookupOrCreate(state map[string]*webhookDeliveryDTO, id string) *webhookDeliveryDTO {
	d, ok := state[id]
	if !ok {
		d = &webhookDeliveryDTO{DeliveryID: id}
		state[id] = d
	}
	return d
}
