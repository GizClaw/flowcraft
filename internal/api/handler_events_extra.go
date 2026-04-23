package api

import (
	"net/http"
	"strconv"

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

// handleEventsSSE answers GET /api/events/stream by delegating to the SSE
// hub wired in bootstrap.WireHubs. Per §6 / §12 the hub guarantees frames
// are byte-equal to the WS / HTTP-pull encodings (event: envelope), with
// 15s `event: heartbeat` keepalives carrying the latest seq.
//
// Required query: partition. Optional: since (default 0). Last-Event-ID
// header overrides since (browser auto-resume).
func (s *Server) handleEventsSSE(w http.ResponseWriter, r *http.Request) {
	// SSEHub is enforced non-nil at NewServer construction (Phase 9).
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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	conn, err := s.deps.SSEHub.Open(r.Context(), actor, partition, since, w)
	if err != nil {
		writeError(w, err)
		return
	}
	conn.Run()
}
