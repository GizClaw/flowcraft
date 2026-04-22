package wshub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// Hub holds the per-process subscriber registry for WebSocket clients.
// It does not subscribe to eventlog directly: every Conn opens its own
// eventlog.Subscription scoped to its partition + actor policy. The Hub only
// tracks live connections so we can broadcast administrative messages
// (heartbeats, shutdown notice) and expose Stop().
type Hub struct {
	log    *eventlog.SQLiteLog
	policy policy.Policy
	cfg    HubConfig

	mu     sync.RWMutex
	conns  map[*Conn]struct{}
	closed atomic.Bool
	stopCh chan struct{}
	hbOnce sync.Once
}

// HubConfig tunes the Hub.
type HubConfig struct {
	HeartbeatInterval time.Duration
	IdleTimeout       time.Duration
	MaxMsgSize        int64
	SendBuffer        int
}

// DefaultHubConfig is the production default.
var DefaultHubConfig = HubConfig{
	HeartbeatInterval: 15 * time.Second,
	IdleTimeout:       60 * time.Second,
	MaxMsgSize:        1 << 20,
	SendBuffer:        256,
}

// NewHub returns a new WebSocket hub.
func NewHub(log *eventlog.SQLiteLog, pol policy.Policy, cfg HubConfig) *Hub {
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = DefaultHubConfig.HeartbeatInterval
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = DefaultHubConfig.IdleTimeout
	}
	if cfg.MaxMsgSize == 0 {
		cfg.MaxMsgSize = DefaultHubConfig.MaxMsgSize
	}
	if cfg.SendBuffer == 0 {
		cfg.SendBuffer = DefaultHubConfig.SendBuffer
	}
	return &Hub{
		log:    log,
		policy: pol,
		cfg:    cfg,
		conns:  make(map[*Conn]struct{}),
		stopCh: make(chan struct{}),
	}
}

// Start kicks off the background heartbeat ticker; safe to call multiple times.
func (h *Hub) Start() {
	h.hbOnce.Do(func() {
		go h.heartbeats()
	})
}

// Stop closes every active connection and stops background goroutines.
func (h *Hub) Stop() {
	if !h.closed.CompareAndSwap(false, true) {
		return
	}
	close(h.stopCh)
	h.mu.Lock()
	conns := make([]*Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

// Open registers a Conn with the Hub. The caller must call Conn.close when
// done, which removes it from the registry.
func (h *Hub) Open(ctx context.Context, actor policy.Actor, partition string, since int64) (*Conn, error) {
	if h.closed.Load() {
		return nil, fmt.Errorf("wshub: hub closed")
	}
	dec, err := h.policy.AllowSubscribe(ctx, actor, policy.SubscribeOptions{
		Partitions: []string{partition},
	})
	if err != nil {
		return nil, fmt.Errorf("wshub: policy error: %w", err)
	}
	if !dec.Allow {
		return nil, fmt.Errorf("wshub: %s", dec.Reason)
	}
	sub, err := h.log.Subscribe(ctx, eventlog.SubscribeOptions{
		Partitions: []string{partition},
		Since:      eventlog.Since(since),
		BufferSize: h.cfg.SendBuffer,
	})
	if err != nil {
		return nil, err
	}
	c := newConn(ctx, h, actor, partition, sub)
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	go c.pumpFromSubscription()
	return c, nil
}

func (h *Hub) removeConn(c *Conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

// ConnectionCount returns the number of live connections (used by /metrics).
func (h *Hub) ConnectionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

func (h *Hub) heartbeats() {
	ticker := time.NewTicker(h.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			latest, _ := h.log.LatestSeq(context.Background())
			payload, _ := json.Marshal(map[string]any{
				"op":         "heartbeat",
				"ts":         time.Now().UTC().Format(time.RFC3339Nano),
				"latest_seq": latest,
			})
			h.mu.RLock()
			for c := range h.conns {
				c.sendControl(payload)
			}
			h.mu.RUnlock()
		case <-h.stopCh:
			return
		}
	}
}

// MarshalEnvelope is re-exported so tests don't need to import eventlog.
func MarshalEnvelope(env eventlog.Envelope) ([]byte, error) {
	return eventlog.MarshalEnvelope(env)
}

// LogSlowConsumer is the slow-consumer hook used by Conn; surfaced for tests.
func LogSlowConsumer(name string, lag int64) {
	slog.Warn("wshub slow consumer", "name", name, "lag", lag)
}
