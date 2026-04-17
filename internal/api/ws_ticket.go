package api

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const wsTicketTTL = 45 * time.Second

type wsTicket struct {
	expiresAt time.Time
}

type wsTicketStore struct {
	mu      sync.Mutex
	tickets map[string]wsTicket
}

func newWSTicketStore() *wsTicketStore {
	return &wsTicketStore{tickets: make(map[string]wsTicket)}
}

func (s *wsTicketStore) issue(ttl time.Duration) (string, time.Time, error) {
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
	}
	return token, expiresAt, nil
}

func (s *wsTicketStore) consume(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ticket, ok := s.tickets[token]
	if !ok {
		return "", false
	}
	delete(s.tickets, token)
	if time.Now().After(ticket.expiresAt) {
		return "", false
	}
	return ownerRealmID, true
}

func (s *wsTicketStore) cleanupExpiredLocked(now time.Time) {
	for token, ticket := range s.tickets {
		if now.After(ticket.expiresAt) {
			delete(s.tickets, token)
		}
	}
}

type wsTicketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (s *Server) handleCreateWSTicket(w http.ResponseWriter, r *http.Request) {
	ticket, expiresAt, err := s.wsTickets.issue(wsTicketTTL)
	if err != nil {
		writeError(w, errdefs.Internalf("failed to issue websocket ticket"))
		return
	}
	writeJSON(w, http.StatusOK, wsTicketResponse{
		Ticket:    ticket,
		ExpiresAt: expiresAt,
	})
}

func (s *Server) authenticateWSRequest(r *http.Request) (string, error) {
	if ticket := r.URL.Query().Get("ticket"); ticket != "" {
		if runtimeID, ok := s.wsTickets.consume(ticket); ok {
			return runtimeID, nil
		}
		return "", errdefs.Unauthorizedf("invalid websocket ticket")
	}
	return "", errdefs.Unauthorizedf("missing websocket ticket")
}
