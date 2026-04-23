package api

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/policy"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const wsTicketTTL = 45 * time.Second

// wsTicket binds a single-use WS authentication token to one (partition,
// since) initial subscription as required by §12.3 of the event-sourcing
// plan. Once consumed by handler_events_ws.go the websocket connection
// auto-subscribes to that exact (partition, since) and refuses any
// subscribe frame whose values diverge.
type wsTicket struct {
	expiresAt time.Time
	actor     policy.Actor
	partition string
	since     int64
}

type wsTicketStore struct {
	mu      sync.Mutex
	tickets map[string]wsTicket
}

func newWSTicketStore() *wsTicketStore {
	return &wsTicketStore{tickets: make(map[string]wsTicket)}
}

// issue mints a new ticket bound to (actor, partition, since). The ticket
// is consumed exactly once; the TTL caps the replay window.
func (s *wsTicketStore) issue(ttl time.Duration, actor policy.Actor, partition string, since int64) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cleanupExpiredLocked(time.Now())

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	expiresAt := time.Now().Add(ttl)
	s.tickets[token] = wsTicket{
		expiresAt: expiresAt,
		actor:     actor,
		partition: partition,
		since:     since,
	}
	return token, expiresAt, nil
}

// consume returns the ticket payload (actor + partition + since) on
// success, or ok=false if the token is unknown or expired. The token is
// always removed from the store.
func (s *wsTicketStore) consume(token string) (wsTicket, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ticket, ok := s.tickets[token]
	if !ok {
		return wsTicket{}, false
	}
	delete(s.tickets, token)
	if time.Now().After(ticket.expiresAt) {
		return wsTicket{}, false
	}
	return ticket, true
}

func (s *wsTicketStore) cleanupExpiredLocked(now time.Time) {
	for token, ticket := range s.tickets {
		if now.After(ticket.expiresAt) {
			delete(s.tickets, token)
		}
	}
}

// validateWSOrigin enforces same-origin (or CORS-allowed origin) on WS
// upgrade requests. It used to live in the deleted handler_ws.go; the
// /api/events/ws handler is now its only caller, so it lives here next
// to the ticket store that gates that endpoint.
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

var _ policy.Actor // keep policy import alive across edits
