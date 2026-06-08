package scriptnode

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
)

const defaultStreamSubBufferSize = 256

// newStreamBridge exposes graph stream-delta subscriptions to scripts as
// the global "stream".
//
// Script-facing API:
//
//	stream.subscribe_node({ node_id: "planner", run_id: "...", buffer_size: 256 })
//	    -> { next, current, close }
func newStreamBridge(defaultRunID string, bus event.Bus) bindings.BindingFunc {
	return func(callCtx context.Context) (string, any) {
		if callCtx == nil {
			callCtx = context.Background()
		}

		return "stream", map[string]any{
			"subscribe_node": func(raw any) (map[string]any, error) {
				if bus == nil {
					return nil, errdefs.NotAvailablef("stream.subscribe_node: event bus not configured")
				}

				opts, err := parseStreamSubscribeOptions(raw, defaultRunID)
				if err != nil {
					return nil, err
				}

				subCtx, cancel := context.WithCancel(callCtx)
				sub, err := bus.Subscribe(
					subCtx,
					engine.PatternRunStream(opts.runID),
					event.WithBufferSize(opts.bufferSize),
					// Keep the publisher path non-blocking when scripts are slow:
					// a full subscription buffer drops the incoming newest delta.
					event.WithBackpressure(event.DropNewest),
					event.WithPredicate(func(env event.Envelope) bool {
						return env.NodeID() == opts.nodeID
					}),
				)
				if err != nil {
					cancel()
					return nil, err
				}

				iter := &streamDeltaIterator{
					ctx:    subCtx,
					cancel: cancel,
					sub:    sub,
				}
				return map[string]any{
					"next":    iter.Next,
					"current": iter.Current,
					"close":   iter.Close,
				}, nil
			},
		}
	}
}

type streamSubscribeOptions struct {
	nodeID     string
	runID      string
	bufferSize int
}

func parseStreamSubscribeOptions(raw any, defaultRunID string) (streamSubscribeOptions, error) {
	opts := streamSubscribeOptions{
		runID:      defaultRunID,
		bufferSize: defaultStreamSubBufferSize,
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return opts, errdefs.Validationf("stream.subscribe_node: options must be an object, got %T", raw)
	}
	if v, ok := m["node_id"].(string); ok {
		opts.nodeID = v
	}
	if opts.nodeID == "" {
		return opts, errdefs.Validationf("stream.subscribe_node: node_id is required")
	}
	if v, ok := m["run_id"]; ok && v != nil {
		s, ok := v.(string)
		if !ok {
			return opts, errdefs.Validationf("stream.subscribe_node: run_id must be a string, got %T", v)
		}
		opts.runID = s
	}
	if opts.runID == "" {
		return opts, errdefs.Validationf("stream.subscribe_node: run_id is required")
	}
	if v, ok := m["buffer_size"]; ok && v != nil {
		n, err := streamBufferSize(v)
		if err != nil {
			return opts, err
		}
		opts.bufferSize = n
	}
	return opts, nil
}

func streamBufferSize(v any) (int, error) {
	switch n := v.(type) {
	case int:
		if n > 0 {
			return n, nil
		}
	case int32:
		if n > 0 {
			return int(n), nil
		}
	case int64:
		if n > 0 {
			return int(n), nil
		}
	case uint:
		if n > 0 {
			return int(n), nil
		}
	case uint32:
		if n > 0 {
			return int(n), nil
		}
	case uint64:
		if n > 0 {
			return int(n), nil
		}
	case float32:
		if n > 0 {
			return int(n), nil
		}
	case float64:
		if n > 0 {
			return int(n), nil
		}
	default:
		return 0, errdefs.Validationf("stream.subscribe_node: buffer_size must be a positive number, got %T", v)
	}
	return 0, errdefs.Validationf("stream.subscribe_node: buffer_size must be positive")
}

type streamDeltaIterator struct {
	ctx    context.Context
	cancel context.CancelFunc
	sub    event.Subscription

	mu      sync.Mutex
	current map[string]any
	once    sync.Once
}

// Next blocks until the next matching delta arrives. It returns false when
// the subscription or parent execution context is closed.
func (i *streamDeltaIterator) Next() bool {
	if i == nil || i.sub == nil {
		return false
	}
	select {
	case env, ok := <-i.sub.C():
		if !ok {
			return false
		}
		i.mu.Lock()
		i.current = streamDeltaEnvelopeToMap(env)
		i.mu.Unlock()
		return true
	case <-i.ctx.Done():
		return false
	}
}

func (i *streamDeltaIterator) Current() map[string]any {
	if i == nil {
		return nil
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.current == nil {
		return nil
	}
	out := make(map[string]any, len(i.current))
	for k, v := range i.current {
		out[k] = v
	}
	return out
}

func (i *streamDeltaIterator) Close() error {
	if i == nil {
		return nil
	}
	var err error
	i.once.Do(func() {
		i.cancel()
		if i.sub != nil {
			err = i.sub.Close()
		}
	})
	return err
}

func streamDeltaEnvelopeToMap(env event.Envelope) map[string]any {
	out := decodeStreamDeltaPayloadMap(env)
	if out == nil {
		out = make(map[string]any, 8)
	}

	// Preserve payload "id" for tool_call deltas; envelope id is always
	// available under envelope_id and also under id when payload.id is absent.
	if _, ok := out["id"]; !ok {
		out["id"] = env.ID
	}
	out["envelope_id"] = env.ID
	out["subject"] = string(env.Subject)
	out["time"] = formatEnvelopeTime(env.Time)
	out["run_id"] = env.RunID()
	out["node_id"] = env.NodeID()
	out["agent_id"] = env.AgentID()
	if env.Source != "" {
		out["source"] = env.Source
	}
	if env.TraceID != "" {
		out["trace_id"] = env.TraceID
	}
	if env.SpanID != "" {
		out["span_id"] = env.SpanID
	}
	return out
}

func decodeStreamDeltaPayloadMap(env event.Envelope) map[string]any {
	if len(env.Payload) == 0 {
		return nil
	}
	var asMap map[string]any
	if err := json.Unmarshal(env.Payload, &asMap); err == nil && asMap != nil {
		return asMap
	}
	p, err := engine.DecodeStreamDelta(env)
	if err == nil {
		return streamDeltaPayloadToMap(p)
	}
	return map[string]any{"raw": string(env.Payload)}
}

func streamDeltaPayloadToMap(p engine.StreamDeltaPayload) map[string]any {
	out := map[string]any{"type": string(p.Type)}
	if p.Content != "" {
		out["content"] = p.Content
	}
	if p.ID != "" {
		out["id"] = p.ID
	}
	if p.Name != "" {
		out["name"] = p.Name
	}
	if p.Arguments != nil {
		out["arguments"] = p.Arguments
	}
	if p.ToolCallID != "" {
		out["tool_call_id"] = p.ToolCallID
	}
	if p.IsError {
		out["is_error"] = true
	}
	if p.Cancelled {
		out["cancelled"] = true
	}
	if p.Speculative {
		out["speculative"] = true
	}
	if p.ForkID != "" {
		out["fork_id"] = p.ForkID
	}
	if p.BranchID != "" {
		out["branch_id"] = p.BranchID
	}
	if p.Reason != "" {
		out["reason"] = p.Reason
	}
	return out
}

func formatEnvelopeTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}
