package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"

	"github.com/coder/websocket"
)

// handleEventsWS answers GET /api/events/ws. Same wire shape as the SSE
// endpoint but multiplexed: clients send `{"type":"subscribe","partition":...,"since":...}`
// frames after connecting and receive `{"type":"envelope","data":{...}}` frames
// back. Heartbeats every 15s carry the latest seq for drift detection.
//
// Auth: same as /api/ws (cookie or ws-ticket) plus per-partition policy check.
func (s *Server) handleEventsWS(w http.ResponseWriter, r *http.Request) {
	if s.deps.EventLog == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{
			"code":    "not_available",
			"message": "event log not configured",
		}})
		return
	}
	if err := s.validateWSOrigin(r); err != nil {
		writeError(w, err)
		return
	}
	actor, ok := policy.ActorFrom(r.Context())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]string{"code": "unauthenticated"}})
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		telemetry.Error(r.Context(), "api: events ws accept", otellog.String("error", err.Error()))
		return
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	ctx := r.Context()
	mux := newEnvelopeWSMux(ctx, conn, s)

	if init := r.URL.Query().Get("partition"); init != "" {
		since := int64(0)
		if v := r.URL.Query().Get("since"); v != "" {
			if n, perr := strconv.ParseInt(v, 10, 64); perr == nil {
				since = n
			}
		}
		mux.subscribe(actor, init, since)
	}

	go mux.heartbeatLoop()

	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			mux.shutdown()
			return
		}
		var frame struct {
			Type      string `json:"type"`
			Partition string `json:"partition"`
			Since     int64  `json:"since"`
		}
		if jerr := json.Unmarshal(raw, &frame); jerr != nil {
			continue
		}
		switch frame.Type {
		case "subscribe":
			mux.subscribe(actor, frame.Partition, frame.Since)
		case "unsubscribe":
			mux.unsubscribe(frame.Partition)
		case "ping":
			_ = mux.write(map[string]any{"type": "pong", "ts": time.Now().UTC().Format(time.RFC3339Nano)})
		}
	}
}

// envelopeWSMux multiplexes per-partition tail loops over a single ws conn.
type envelopeWSMux struct {
	ctx     context.Context
	conn    *websocket.Conn
	srv     *Server
	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	writeMu sync.Mutex
}

func newEnvelopeWSMux(ctx context.Context, conn *websocket.Conn, srv *Server) *envelopeWSMux {
	return &envelopeWSMux{
		ctx:     ctx,
		conn:    conn,
		srv:     srv,
		cancels: make(map[string]context.CancelFunc),
	}
}

func (m *envelopeWSMux) subscribe(actor policy.Actor, partition string, since int64) {
	if partition == "" {
		return
	}
	if pol, ok := m.srv.policyOrNil(); ok {
		dec, err := pol.AllowRead(m.ctx, actor, policy.ReadOptions{Partitions: []string{partition}})
		if err != nil || !dec.Allow {
			_ = m.write(map[string]any{
				"type":      "error",
				"partition": partition,
				"code":      "forbidden",
			})
			return
		}
	}
	m.mu.Lock()
	if cancel, ok := m.cancels[partition]; ok {
		cancel()
	}
	subCtx, cancel := context.WithCancel(m.ctx)
	m.cancels[partition] = cancel
	m.mu.Unlock()
	go m.tail(subCtx, partition, since)
}

func (m *envelopeWSMux) unsubscribe(partition string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel, ok := m.cancels[partition]; ok {
		cancel()
		delete(m.cancels, partition)
	}
}

func (m *envelopeWSMux) shutdown() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, cancel := range m.cancels {
		cancel()
	}
	m.cancels = map[string]context.CancelFunc{}
}

func (m *envelopeWSMux) tail(ctx context.Context, partition string, since int64) {
	pollEvery := 250 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		res, err := m.srv.deps.EventLog.Read(ctx, partition, eventlog.Since(since), 200)
		if err != nil {
			return
		}
		for _, env := range res.Events {
			body, err := eventlog.MarshalEnvelope(env)
			if err != nil {
				continue
			}
			frame := map[string]any{
				"type":      "envelope",
				"partition": partition,
				"data":      json.RawMessage(body),
			}
			if werr := m.write(frame); werr != nil {
				return
			}
			since = env.Seq
		}
		if res.HasMore {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollEvery):
		}
	}
}

func (m *envelopeWSMux) heartbeatLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			latest, _ := m.srv.deps.EventLog.LatestSeq(m.ctx)
			if err := m.write(map[string]any{
				"type":       "heartbeat",
				"latest_seq": latest,
				"ts":         time.Now().UTC().Format(time.RFC3339Nano),
			}); err != nil {
				return
			}
		}
	}
}

func (m *envelopeWSMux) write(payload any) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return m.conn.Write(m.ctx, websocket.MessageText, body)
}
