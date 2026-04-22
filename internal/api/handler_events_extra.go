package api

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// handleEventsLatestSeq answers GET /api/events/latest-seq.
// Optional partition query param. Used by the EnvelopeClient on connect to
// detect drift before subscribing.
func (s *Server) handleEventsLatestSeq(w http.ResponseWriter, r *http.Request) {
	if s.deps.EventLog == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{
			"code":    "not_available",
			"message": "event log not configured",
		}})
		return
	}
	latest, err := s.deps.EventLog.LatestSeq(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"latest_seq": latest})
}

// handleEventsSSE answers GET /api/events/stream. SSE envelope frames; one
// `data:` line per envelope, byte-equal to the WS / HTTP-pull encoding. The
// `event:` field is always "envelope" except for periodic `heartbeat` frames
// (15s cadence) so the client can detect a wedged connection.
//
// Required query: partition. Optional: since (default 0). Last-Event-ID
// header overrides since (browser auto-resume).
func (s *Server) handleEventsSSE(w http.ResponseWriter, r *http.Request) {
	if s.deps.EventLog == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{
			"code":    "not_available",
			"message": "event log not configured",
		}})
		return
	}
	actor, ok := policy.ActorFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "unauthenticated"}})
		return
	}
	partition := r.URL.Query().Get("partition")
	if partition == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{
			"code":    "bad_request",
			"message": "partition required",
		}})
		return
	}
	if pol, ok := s.policyOrNil(); ok {
		dec, err := pol.AllowRead(r.Context(), actor, policy.ReadOptions{Partitions: []string{partition}})
		if err != nil {
			writeError(w, err)
			return
		}
		if !dec.Allow {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{
				"code":    "forbidden",
				"message": dec.Reason,
			}})
			return
		}
	}

	since := int64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = n
		}
	} else if v := r.URL.Query().Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			since = n
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": map[string]string{
			"code":    "internal_error",
			"message": "streaming unsupported",
		}})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if err := s.streamEnvelopes(r.Context(), w, flusher, partition, since); err != nil {
		// connection closed; nothing to write
		return
	}
}

// policyOrNil returns the configured policy or false if not wired.
func (s *Server) policyOrNil() (policy.Policy, bool) {
	if s.deps.Policy == nil {
		return nil, false
	}
	return s.deps.Policy, true
}

// streamEnvelopes pumps envelope frames into the response writer until the
// request context cancels. Heartbeats every 15s carry the latest seq so the
// client detects silent gaps.
func (s *Server) streamEnvelopes(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, partition string, since int64) error {
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	pollEvery := 250 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-heartbeat.C:
			latest, _ := s.deps.EventLog.LatestSeq(ctx)
			if _, err := fmt.Fprintf(w, "event: heartbeat\ndata: {\"latest_seq\":%d,\"ts\":%q}\n\n",
				latest, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
			flusher.Flush()
		default:
		}

		res, err := s.deps.EventLog.Read(ctx, partition, eventlog.Since(since), 200)
		if err != nil {
			return err
		}
		for _, env := range res.Events {
			body, err := eventlog.MarshalEnvelope(env)
			if err != nil {
				continue
			}
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "id: %d\nevent: envelope\ndata: ", env.Seq)
			buf.Write(body)
			buf.WriteString("\n\n")
			if _, err := w.Write(buf.Bytes()); err != nil {
				return err
			}
			flusher.Flush()
			since = env.Seq
		}
		if res.HasMore {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollEvery):
		}
	}
}
