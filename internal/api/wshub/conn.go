package wshub

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/policy"
	"github.com/coder/websocket"
)

// Conn is one logical WebSocket subscriber. The Conn owns:
//   - the eventlog.Subscription (sub) that supplies envelopes
//   - the writeLoop goroutine that drains sub.C() into c.out
//   - the readLoop goroutine that handles client control frames
//
// Wire format on the WS channel: each envelope is sent as a single text
// frame whose body is exactly eventlog.MarshalEnvelope(env), so byte-equal
// to the SSE/HTTP-pull representations. Control messages (heartbeat,
// errors) carry an "op" field so clients can dispatch.
type Conn struct {
	ctx       context.Context
	hub       *Hub
	actor     policy.Actor
	partition string
	sub       eventlog.Subscription
	conn      *websocket.Conn

	out       chan []byte
	dropped   atomic.Int64
	done      chan struct{}
	closeOnce sync.Once
}

func newConn(ctx context.Context, hub *Hub, actor policy.Actor, partition string, sub eventlog.Subscription) *Conn {
	return &Conn{
		ctx:       ctx,
		hub:       hub,
		actor:     actor,
		partition: partition,
		sub:       sub,
		out:       make(chan []byte, hub.cfg.SendBuffer),
		done:      make(chan struct{}),
	}
}

// Attach takes ownership of an upgraded websocket.Conn and runs the read+
// write loops. It blocks until the connection is closed.
func (c *Conn) Attach(ws *websocket.Conn) {
	c.conn = ws
	if c.hub.cfg.MaxMsgSize > 0 {
		ws.SetReadLimit(c.hub.cfg.MaxMsgSize)
	}
	go c.readLoop()
	c.writeLoop()
}

// pumpFromSubscription drains the eventlog subscription into c.out using the
// canonical envelope serializer. It runs as soon as Hub.Open registers the
// connection; closing the subscription unblocks it.
func (c *Conn) pumpFromSubscription() {
	for {
		select {
		case env, ok := <-c.sub.C():
			if !ok {
				c.close()
				return
			}
			data, err := eventlog.MarshalEnvelope(env)
			if err != nil {
				slog.Warn("wshub: marshal envelope", "err", err)
				continue
			}
			c.send(data)
		case <-c.done:
			return
		case <-c.ctx.Done():
			c.close()
			return
		}
	}
}

func (c *Conn) readLoop() {
	defer c.close()
	for {
		_, msg, err := c.conn.Read(c.ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				slog.Debug("wshub readLoop end", "err", err)
			}
			return
		}
		c.handle(msg)
	}
}

func (c *Conn) writeLoop() {
	defer c.close()
	for {
		select {
		case data, ok := <-c.out:
			if !ok {
				return
			}
			if c.conn == nil {
				continue
			}
			if err := c.conn.Write(c.ctx, websocket.MessageText, data); err != nil {
				slog.Debug("wshub writeLoop end", "err", err)
				return
			}
		case <-c.done:
			return
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Conn) handle(msg []byte) {
	var op struct {
		Op string `json:"op"`
	}
	if err := json.Unmarshal(msg, &op); err != nil {
		c.sendControlErr("bad_message", "invalid JSON")
		return
	}
	switch op.Op {
	case "ping":
		c.sendControl([]byte(`{"op":"pong"}`))
	}
}

// send queues a raw envelope payload. Drops with a counter increment if the
// buffer is full, never blocking the eventlog goroutine.
func (c *Conn) send(data []byte) {
	select {
	case c.out <- data:
	case <-c.done:
	default:
		n := c.dropped.Add(1)
		if n%100 == 1 {
			LogSlowConsumer(c.partition, n)
		}
	}
}

// sendControl queues an administrative payload (heartbeat, pong, error).
func (c *Conn) sendControl(data []byte) { c.send(data) }

func (c *Conn) sendControlErr(code, msg string) {
	body, _ := json.Marshal(map[string]any{"op": "error", "code": code, "msg": msg})
	c.sendControl(body)
}

// close releases all resources; idempotent.
func (c *Conn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		if c.sub != nil {
			_ = c.sub.Close()
		}
		c.hub.removeConn(c)
		if c.conn != nil {
			_ = c.conn.Close(websocket.StatusNormalClosure, "")
		}
		close(c.out)
	})
}

// Out returns the raw outbound channel; tests use it to assert on the exact
// bytes the connection would write to the wire.
func (c *Conn) Out() <-chan []byte { return c.out }

// Done is closed when the connection is fully torn down.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Dropped returns the count of envelopes dropped due to slow consumer.
func (c *Conn) Dropped() int64 { return c.dropped.Load() }
