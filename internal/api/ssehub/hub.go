package ssehub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
)

// Hub manages live SSE subscribers. Each Conn owns its own
// eventlog.Subscription; the Hub itself is just a registry so admin code can
// see how many clients are connected and shut them down on Stop.
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

// HubConfig tunes the SSE hub.
type HubConfig struct {
	HeartbeatInterval time.Duration
	FlushInterval     time.Duration
	SendBuffer        int
}

// DefaultHubConfig is production default.
var DefaultHubConfig = HubConfig{
	HeartbeatInterval: 15 * time.Second,
	FlushInterval:     100 * time.Millisecond,
	SendBuffer:        256,
}

// NewHub returns a new SSE hub.
func NewHub(log *eventlog.SQLiteLog, pol policy.Policy, cfg HubConfig) *Hub {
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = DefaultHubConfig.HeartbeatInterval
	}
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = DefaultHubConfig.FlushInterval
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

// Start kicks the heartbeat goroutine.
func (h *Hub) Start() { h.hbOnce.Do(func() { go h.heartbeats() }) }

// Stop closes all connections and stops background goroutines.
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
		c.Close()
	}
}

// Open authorises the actor + partition and spawns a Conn that streams
// envelopes from the eventlog into w. The caller MUST run Conn.Run on the
// HTTP goroutine; Open returns once registration is complete.
func (h *Hub) Open(ctx context.Context, actor policy.Actor, partition string, since int64, w io.Writer) (*Conn, error) {
	if h.closed.Load() {
		return nil, fmt.Errorf("ssehub: hub closed")
	}
	dec, err := h.policy.AllowSubscribe(ctx, actor, policy.SubscribeOptions{
		Partitions: []string{partition},
	})
	if err != nil {
		return nil, fmt.Errorf("ssehub: policy error: %w", err)
	}
	if !dec.Allow {
		return nil, fmt.Errorf("ssehub: %s", dec.Reason)
	}
	sub, err := h.log.Subscribe(ctx, eventlog.SubscribeOptions{
		Partitions: []string{partition},
		Since:      eventlog.Since(since),
		BufferSize: h.cfg.SendBuffer,
	})
	if err != nil {
		return nil, err
	}
	c := &Conn{
		hub:       h,
		partition: partition,
		w:         w,
		sub:       sub,
		ctx:       ctx,
		done:      make(chan struct{}),
	}
	if f, ok := w.(http.Flusher); ok {
		c.flusher = f
	}
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
	return c, nil
}

// ConnectionCount returns the live SSE connection count.
func (h *Hub) ConnectionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.conns)
}

func (h *Hub) removeConn(c *Conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

func (h *Hub) heartbeats() {
	ticker := time.NewTicker(h.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			latest, _ := h.log.LatestSeq(context.Background())
			h.mu.RLock()
			for c := range h.conns {
				c.writeHeartbeat(latest)
			}
			h.mu.RUnlock()
		case <-h.stopCh:
			return
		}
	}
}

// Conn streams envelopes for one partition to one HTTP response writer.
type Conn struct {
	hub       *Hub
	partition string
	w         io.Writer
	flusher   http.Flusher
	sub       eventlog.Subscription
	ctx       context.Context

	mu        sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
}

// Run blocks the HTTP goroutine, writing each envelope as a single SSE event.
// The data field is exactly eventlog.MarshalEnvelope(env) so it byte-equals
// the WS / HTTP-pull representations.
func (c *Conn) Run() {
	defer c.Close()
	for {
		select {
		case env, ok := <-c.sub.C():
			if !ok {
				return
			}
			if err := c.writeEvent(env); err != nil {
				if !errors.Is(err, context.Canceled) {
					slog.Debug("ssehub write end", "err", err)
				}
				return
			}
		case <-c.ctx.Done():
			return
		case <-c.done:
			return
		}
	}
}

func (c *Conn) writeEvent(env eventlog.Envelope) error {
	body, err := eventlog.MarshalEnvelope(env)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.w, "id: %d\nevent: %s\ndata: %s\n\n", env.Seq, env.Type, body); err != nil {
		return err
	}
	if c.flusher != nil {
		c.flusher.Flush()
	}
	return nil
}

func (c *Conn) writeHeartbeat(latest int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.w, ": heartbeat latest_seq=%d\n\n", latest)
	if c.flusher != nil {
		c.flusher.Flush()
	}
}

// Close stops streaming and removes the connection from the hub registry.
func (c *Conn) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		if c.sub != nil {
			_ = c.sub.Close()
		}
		c.hub.removeConn(c)
	})
}

// Done is closed when the connection is fully torn down.
func (c *Conn) Done() <-chan struct{} { return c.done }
