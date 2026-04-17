package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/GizClaw/flowcraft/internal/stream"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type wsMessage struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if err := s.validateWSOrigin(r); err != nil {
		writeError(w, err)
		return
	}
	runtimeID, err := s.authenticateWSRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		telemetry.Error(r.Context(), "api: websocket accept", otellog.String("error", err.Error()))
		return
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	ctx := r.Context()

	if runtimeID != "" {
		go func() {
			board := s.waitRuntimeBoard(ctx, runtimeID)
			if board == nil {
				return
			}
			ch := board.Watch(ctx)
			for card := range ch {
				if card.Type == "result" {
					continue
				}
				_ = wsjson.Write(ctx, conn, map[string]any{
					"type":    "kanban",
					"payload": cardToKanbanEvent(card),
				})
			}
		}()
		go s.bridgeSubAgentStreamBoard(ctx, conn, s.waitRuntimeBoard(ctx, runtimeID))
	}

	if s.deps.Platform.EventBus != nil {
		go func() {
			sub, err := s.deps.Platform.EventBus.Subscribe(ctx, event.EventFilter{
				Types: []event.EventType{event.EventGraphChanged, event.EventCompileResult},
			})
			if err != nil {
				return
			}
			defer func() { _ = sub.Close() }()
			stream.StreamLoop(ctx, sub, nil, &wsSink{conn: conn, ctx: ctx})
		}()
	}

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = wsjson.Write(ctx, conn, map[string]any{"type": "pong"})
			}
		}
	}()

	for {
		var msg wsMessage
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			return
		}
		switch msg.Type {
		case "ping":
			_ = wsjson.Write(ctx, conn, map[string]any{"type": "pong"})
		}
	}
}

func (s *Server) validateWSOrigin(r *http.Request) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	if len(s.config.CORSOrigins) > 0 {
		for _, allowed := range s.config.CORSOrigins {
			if allowed == origin {
				return nil
			}
		}
		return errdefs.Forbiddenf("invalid websocket origin")
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return errdefs.Forbiddenf("invalid websocket origin")
	}
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	if parsed.Scheme != scheme || parsed.Host != r.Host {
		return errdefs.Forbiddenf("invalid websocket origin")
	}
	return nil
}

func (s *Server) waitRuntimeBoard(_ context.Context, _ string) *kanban.TaskBoard {
	return s.deps.Platform.TaskBoard()
}

func cardToKanbanEvent(card *kanban.Card) map[string]any {
	var eventType string
	switch card.Status {
	case kanban.CardPending:
		eventType = "card_created"
	case kanban.CardClaimed:
		eventType = "card_claimed"
	case kanban.CardDone:
		eventType = "card_done"
	case kanban.CardFailed:
		eventType = "card_failed"
	default:
		eventType = "card_created"
	}
	cardInfo := map[string]any{
		"id": card.ID, "type": card.Type, "status": string(card.Status),
		"producer": card.Producer, "consumer": card.Consumer,
		"created_at": card.CreatedAt, "updated_at": card.UpdatedAt,
	}
	if card.Error != "" {
		cardInfo["error"] = card.Error
	}
	if card.Meta != nil {
		cardInfo["meta"] = card.Meta
	}
	if card.UpdatedAt.After(card.CreatedAt) {
		cardInfo["elapsed_ms"] = card.UpdatedAt.Sub(card.CreatedAt).Milliseconds()
	}
	if card.Payload != nil {
		extractWSPayload(card.Payload, cardInfo)
	}
	return map[string]any{"type": eventType, "card": cardInfo, "timestamp": card.UpdatedAt}
}

func extractWSPayload(payload any, out map[string]any) {
	switch p := payload.(type) {
	case map[string]any:
		if q, _ := p["query"].(string); q != "" {
			out["query"] = q
		}
		if t, _ := p["target_agent_id"].(string); t != "" {
			out["target_agent_id"] = t
		}
		if o, _ := p["output"].(string); o != "" {
			out["output"] = o
		}
		if r, _ := p["run_id"].(string); r != "" {
			out["run_id"] = r
		}
	default:
		data, err := json.Marshal(p)
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			return
		}
		extractWSPayload(m, out)
	}
}

func (s *Server) bridgeSubAgentStreamBoard(ctx context.Context, conn *websocket.Conn, sb *kanban.TaskBoard) {
	if sb == nil {
		return
	}
	sub, err := sb.Bus().Subscribe(ctx, event.EventFilter{
		Types: []event.EventType{
			event.EventStreamDelta, event.EventGraphStart,
			event.EventType(kanban.EventCallbackStart),
			event.EventType(kanban.EventCallbackDone),
		},
	})
	if err != nil {
		return
	}
	defer func() { _ = sub.Close() }()
	stream.StreamLoop(ctx, sub, nil, &wsSink{conn: conn, ctx: ctx}, CardIDEnricher(sb))
}

// CardIDEnricher returns an EnrichFunc that annotates mapped events with the
// active card ID from the given TaskBoard, enabling the frontend to correlate
// streamed deltas with specific kanban cards.
func CardIDEnricher(board *kanban.TaskBoard) stream.EnrichFunc {
	return func(ev event.Event, mapped *stream.MappedEvent) bool {
		if board == nil {
			return true
		}
		if ev.ActorID != "" {
			cards := board.Query(kanban.CardFilter{Consumer: ev.ActorID, Status: kanban.CardClaimed})
			if len(cards) > 0 {
				mapped.Payload["card_id"] = cards[0].ID
			}
		}
		return true
	}
}
