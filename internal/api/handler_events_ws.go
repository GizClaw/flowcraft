package api

import (
	"net/http"

	"github.com/GizClaw/flowcraft/internal/policy"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/coder/websocket"
)

// handleEventsWS answers GET /api/events/ws by delegating to the wshub
// wired in bootstrap.WireHubs. The hub owns the per-connection
// subscribe/unsubscribe protocol and emits {"type":"envelope","data":<env>}
// frames byte-equal to the SSE / HTTP-pull encodings (§6.5 / §12).
//
// Auth: a §12.3 ws-ticket is required (single-use, bound to (actor,
// partition, since)). The ticket is consumed here and the resulting
// (partition, since) drives the initial subscription via OpenWithInitial,
// so the very first frame after the websocket upgrade is already an
// envelope with no client-side round-trip.
func (s *Server) handleEventsWS(w http.ResponseWriter, r *http.Request) {
	// WSHub is enforced non-nil at NewServer construction (Phase 9).
	if err := s.validateWSOrigin(r); err != nil {
		writeError(w, err)
		return
	}
	token := r.URL.Query().Get("ticket")
	if token == "" {
		writeError(w, errdefs.Unauthorizedf("missing websocket ticket"))
		return
	}
	ticket, ok := s.wsTickets.consume(token)
	if !ok {
		writeError(w, errdefs.Unauthorizedf("invalid websocket ticket"))
		return
	}

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		telemetry.Error(r.Context(), "api: events ws accept", otellog.String("error", err.Error()))
		return
	}

	// Run the hub-managed conn under the ticket-bound actor; the
	// request context already carries unauth principals which we
	// override here with the ticket's bound actor (per §12.3 the
	// ticket is the authoritative source of identity for /events/ws).
	ctx := policy.WithActor(r.Context(), ticket.actor)
	hubConn, err := s.deps.WSHub.OpenWithInitial(ctx, ticket.actor, ticket.partition, ticket.since)
	if err != nil {
		_ = wsConn.Close(websocket.StatusPolicyViolation, err.Error())
		return
	}
	hubConn.Attach(wsConn)
}
