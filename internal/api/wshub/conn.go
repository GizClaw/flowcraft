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
//   - a set of partition subscriptions (subs), each one an independent
//     eventlog.Subscription pumping into c.out
//   - the writeLoop goroutine that drains c.out into the websocket
//   - the readLoop goroutine that handles client control frames
//
// Wire format:
//
//	{"type":"envelope","data":<eventlog.MarshalEnvelope(env)>}
//
// where the data field is the raw envelope JSON, byte-equal to the SSE /
// HTTP-pull representations. Control frames carry a "type" of
// "subscribed" / "unsubscribed" / "heartbeat" / "error" / "pong".
//
// Client control frames the Conn understands:
//
//	{"type":"subscribe","partition":"<kind:id>","since":<int64>}
//	{"type":"unsubscribe","partition":"<kind:id>"}
//	{"type":"ping"}
//
// Subscriptions are scoped to the Conn's actor. Each subscribe goes
// through Policy.AllowSubscribe; rejected requests result in a frame
// {"type":"error","code":"forbidden","partition":"...","message":"..."}.
type Conn struct {
	ctx   context.Context
	hub   *Hub
	actor policy.Actor
	conn  *websocket.Conn

	out       chan []byte
	dropped   atomic.Int64
	done      chan struct{}
	closeOnce sync.Once

	// subs is the live partition→subscription map. Only the readLoop
	// goroutine writes to it; pump goroutines hold their own subscription
	// reference for the lifetime of the goroutine, so it is safe to
	// delete here without coordination.
	subsMu sync.Mutex
	subs   map[string]*partitionSub
}

// partitionSub bundles a Subscription with the cancel func for its pump
// goroutine.
type partitionSub struct {
	sub    eventlog.Subscription
	cancel context.CancelFunc
}

func newConn(ctx context.Context, hub *Hub, actor policy.Actor) *Conn {
	return &Conn{
		ctx:   ctx,
		hub:   hub,
		actor: actor,
		out:   make(chan []byte, hub.cfg.SendBuffer),
		done:  make(chan struct{}),
		subs:  make(map[string]*partitionSub),
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

// SubscribePartition is the programmatic equivalent of receiving a
// {"type":"subscribe"} frame. Tests that don't want to round-trip
// websocket frames use it directly.
func (c *Conn) SubscribePartition(partition string, since int64) error {
	dec, err := c.hub.policy.AllowSubscribe(c.ctx, c.actor, policy.SubscribeOptions{
		Partitions: []string{partition},
	})
	if err != nil {
		return err
	}
	if !dec.Allow {
		return errors.New(dec.Reason)
	}
	c.subsMu.Lock()
	if _, exists := c.subs[partition]; exists {
		c.subsMu.Unlock()
		return nil
	}
	pumpCtx, cancel := context.WithCancel(c.ctx)
	sub, err := c.hub.log.Subscribe(pumpCtx, eventlog.SubscribeOptions{
		Partitions: []string{partition},
		Since:      eventlog.Since(since),
		BufferSize: c.hub.cfg.SendBuffer,
	})
	if err != nil {
		cancel()
		c.subsMu.Unlock()
		return err
	}
	c.subs[partition] = &partitionSub{sub: sub, cancel: cancel}
	c.subsMu.Unlock()
	go c.pumpFromSubscription(partition, sub)
	c.sendControl(map[string]any{"type": "subscribed", "partition": partition})
	return nil
}

// UnsubscribePartition stops the pump for partition. Idempotent.
func (c *Conn) UnsubscribePartition(partition string) {
	c.subsMu.Lock()
	ps, ok := c.subs[partition]
	delete(c.subs, partition)
	c.subsMu.Unlock()
	if !ok {
		return
	}
	ps.cancel()
	_ = ps.sub.Close()
	c.sendControl(map[string]any{"type": "unsubscribed", "partition": partition})
}

// pumpFromSubscription drains one subscription into c.out. Wrapping the
// envelope in {"type":"envelope","data":<bytes>} keeps the data field
// byte-equal to the SSE / HTTP-pull representations (§6.5).
func (c *Conn) pumpFromSubscription(partition string, sub eventlog.Subscription) {
	_ = partition // reserved for slow-consumer accounting; see Phase 9.
	for {
		select {
		case env, ok := <-sub.C():
			if !ok {
				return
			}
			data, err := eventlog.MarshalEnvelope(env)
			if err != nil {
				slog.Warn("wshub: marshal envelope", "err", err)
				continue
			}
			frame, err := encodeEnvelopeFrame(data)
			if err != nil {
				slog.Warn("wshub: encode envelope frame", "err", err)
				continue
			}
			c.send(frame)
		case <-c.done:
			return
		case <-c.ctx.Done():
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

// handle dispatches a single client control frame.
func (c *Conn) handle(msg []byte) {
	var op struct {
		Type      string `json:"type"`
		Partition string `json:"partition"`
		Since     int64  `json:"since"`
	}
	if err := json.Unmarshal(msg, &op); err != nil {
		c.sendControlErr("bad_message", "", "invalid JSON")
		return
	}
	switch op.Type {
	case "ping":
		c.sendControl(map[string]any{"type": "pong"})
	case "subscribe":
		if op.Partition == "" {
			c.sendControlErr("bad_request", "", "partition required")
			return
		}
		if err := c.SubscribePartition(op.Partition, op.Since); err != nil {
			c.sendControlErr("forbidden", op.Partition, err.Error())
		}
	case "unsubscribe":
		if op.Partition == "" {
			c.sendControlErr("bad_request", "", "partition required")
			return
		}
		c.UnsubscribePartition(op.Partition)
	default:
		c.sendControlErr("bad_message", "", "unknown type "+op.Type)
	}
}

// send queues a raw payload. Drops with a counter increment if the buffer
// is full so the eventlog goroutine never blocks on a slow consumer.
func (c *Conn) send(data []byte) {
	select {
	case c.out <- data:
	case <-c.done:
	default:
		n := c.dropped.Add(1)
		if n%100 == 1 {
			LogSlowConsumer(c.actor.ID, n)
		}
	}
}

// sendControl queues an administrative payload (subscribed, heartbeat,
// pong, error). Marshalling failures are logged and silently swallowed.
func (c *Conn) sendControl(payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		slog.Debug("wshub: encode control", "err", err)
		return
	}
	c.send(body)
}

func (c *Conn) sendControlErr(code, partition, msg string) {
	payload := map[string]any{"type": "error", "code": code, "message": msg}
	if partition != "" {
		payload["partition"] = partition
	}
	c.sendControl(payload)
}

// broadcastHeartbeat is invoked by Hub.heartbeats; the heartbeat frame
// is the same envelope-shaped JSON {"type":"heartbeat","latest_seq":N}
// the SSE channel uses.
func (c *Conn) broadcastHeartbeat(latestSeq int64) {
	c.sendControl(map[string]any{
		"type":       "heartbeat",
		"latest_seq": latestSeq,
	})
}

// close releases all resources; idempotent.
func (c *Conn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.subsMu.Lock()
		for _, ps := range c.subs {
			ps.cancel()
			_ = ps.sub.Close()
		}
		c.subs = nil
		c.subsMu.Unlock()
		c.hub.removeConn(c)
		if c.conn != nil {
			_ = c.conn.Close(websocket.StatusNormalClosure, "")
		}
		close(c.out)
	})
}

// Out returns the raw outbound channel; tests use it to assert on the
// exact bytes the connection would write to the wire.
func (c *Conn) Out() <-chan []byte { return c.out }

// Done is closed when the connection is fully torn down.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Dropped returns the count of payloads dropped due to slow consumer.
func (c *Conn) Dropped() int64 { return c.dropped.Load() }

// encodeEnvelopeFrame wraps a raw envelope JSON in the on-wire control
// frame {"type":"envelope","data":<env>}. We embed `data` as raw JSON so
// the bytes inside `data` remain byte-equal to the SSE/HTTP-pull
// representations, satisfying the §6.5 wire-format invariant.
func encodeEnvelopeFrame(envBytes []byte) ([]byte, error) {
	out := make([]byte, 0, len(envBytes)+24)
	out = append(out, []byte(`{"type":"envelope","data":`)...)
	out = append(out, envBytes...)
	out = append(out, '}')
	return out, nil
}
