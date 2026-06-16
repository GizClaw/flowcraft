package scriptnode

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/script"
)

const (
	defaultStreamSubBufferSize = 256
	maxStreamSubBufferSize     = 4096
)

type streamCleanupKey struct{}

type streamCleanupRegistry struct {
	mu       sync.Mutex
	cleanups []func() error
}

func withStreamCleanup(ctx context.Context) (context.Context, *streamCleanupRegistry) {
	reg := &streamCleanupRegistry{}
	return context.WithValue(ctx, streamCleanupKey{}, reg), reg
}

func streamCleanupFromContext(ctx context.Context) *streamCleanupRegistry {
	reg, _ := ctx.Value(streamCleanupKey{}).(*streamCleanupRegistry)
	return reg
}

func (r *streamCleanupRegistry) Add(fn func() error) {
	if r == nil || fn == nil {
		return
	}
	r.mu.Lock()
	r.cleanups = append(r.cleanups, fn)
	r.mu.Unlock()
}

func (r *streamCleanupRegistry) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cleanups := append([]func() error(nil), r.cleanups...)
	r.cleanups = nil
	r.mu.Unlock()
	for i := len(cleanups) - 1; i >= 0; i-- {
		_ = cleanups[i]()
	}
}

// newStreamBridge exposes graph node stream subscriptions to scripts as
// the global "stream".
//
// Script-facing API:
//
//	stream.subscribe_node({ node_id: "planner", run_id: "...", buffer_size: 256 })
//	stream.subscribe_node({ node_ids: ["planner", "executor"] })
//	    -> { next, next_timeout_ms, current, close }
func newStreamBridge(defaultRunID string, bus event.Bus) script.BindingFunc {
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
					engine.PatternRun(opts.runID),
					event.WithBufferSize(opts.bufferSize),
					// Keep the publisher path non-blocking when scripts are slow:
					// a full subscription buffer drops older events so terminal
					// lifecycle events still reach slow subscribers.
					event.WithBackpressure(event.DropOldest),
					event.WithPredicate(func(env event.Envelope) bool {
						_, ok := opts.nodeIDSet[env.NodeID()]
						return ok
					}),
				)
				if err != nil {
					cancel()
					return nil, err
				}

				iter := &streamNodeIterator{
					ctx:    subCtx,
					cancel: cancel,
					sub:    sub,

					targetNodeCount: len(opts.nodeIDSet),
					terminalNodes:   make(map[string]struct{}, len(opts.nodeIDSet)),
				}
				if reg := streamCleanupFromContext(callCtx); reg != nil {
					reg.Add(iter.Close)
				}
				return map[string]any{
					"next":            iter.Next,
					"next_timeout_ms": iter.NextTimeoutMS,
					"current":         iter.Current,
					"close":           iter.Close,
				}, nil
			},
		}
	}
}

type streamSubscribeOptions struct {
	nodeID     string
	nodeIDs    []string
	nodeIDSet  map[string]struct{}
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
	nodeIDRaw, hasNodeID := m["node_id"]
	nodeIDsRaw, hasNodeIDs := m["node_ids"]
	if hasNodeID && hasNodeIDs {
		return opts, errdefs.Validationf("stream.subscribe_node: node_id and node_ids are mutually exclusive")
	}
	switch {
	case hasNodeID:
		s, ok := nodeIDRaw.(string)
		if !ok {
			return opts, errdefs.Validationf("stream.subscribe_node: node_id must be a string, got %T", nodeIDRaw)
		}
		if s == "" {
			return opts, errdefs.Validationf("stream.subscribe_node: node_id must be a non-empty string")
		}
		opts.nodeID = s
		opts.nodeIDs = []string{s}
	case hasNodeIDs:
		ids, err := streamNodeIDs(nodeIDsRaw)
		if err != nil {
			return opts, err
		}
		opts.nodeIDs = ids
	default:
		return opts, errdefs.Validationf("stream.subscribe_node: node_id or node_ids is required")
	}
	opts.nodeIDSet = make(map[string]struct{}, len(opts.nodeIDs))
	for _, nodeID := range opts.nodeIDs {
		opts.nodeIDSet[nodeID] = struct{}{}
	}
	if v, ok := m["run_id"]; ok && v != nil {
		s, ok := v.(string)
		if !ok {
			return opts, errdefs.Validationf("stream.subscribe_node: run_id must be a string, got %T", v)
		}
		if s != defaultRunID {
			return opts, errdefs.Validationf("stream.subscribe_node: run_id override is not allowed")
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

func streamNodeIDs(v any) ([]string, error) {
	var raw []any
	switch ids := v.(type) {
	case []string:
		out := append([]string(nil), ids...)
		return validateStreamNodeIDs(out)
	case []any:
		raw = ids
	default:
		return nil, errdefs.Validationf("stream.subscribe_node: node_ids must be an array of strings, got %T", v)
	}
	if len(raw) == 0 {
		return nil, errdefs.Validationf("stream.subscribe_node: node_ids must not be empty")
	}
	out := make([]string, 0, len(raw))
	for idx, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, errdefs.Validationf("stream.subscribe_node: node_ids[%d] must be a string, got %T", idx, item)
		}
		out = append(out, s)
	}
	return validateStreamNodeIDs(out)
}

func validateStreamNodeIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, errdefs.Validationf("stream.subscribe_node: node_ids must not be empty")
	}
	for idx, nodeID := range ids {
		if nodeID == "" {
			return nil, errdefs.Validationf("stream.subscribe_node: node_ids[%d] must be a non-empty string", idx)
		}
	}
	return ids, nil
}

func streamBufferSize(v any) (int, error) {
	switch n := v.(type) {
	case int:
		if n > 0 && n <= maxStreamSubBufferSize {
			return n, nil
		}
	case int32:
		if n > 0 && n <= maxStreamSubBufferSize {
			return int(n), nil
		}
	case int64:
		if n > 0 && n <= maxStreamSubBufferSize {
			return int(n), nil
		}
	case uint:
		if n > 0 && n <= maxStreamSubBufferSize {
			return int(n), nil
		}
	case uint32:
		if n > 0 && n <= maxStreamSubBufferSize {
			return int(n), nil
		}
	case uint64:
		if n > 0 && n <= maxStreamSubBufferSize {
			return int(n), nil
		}
	case float32:
		if math.Trunc(float64(n)) != float64(n) {
			return 0, errdefs.Validationf("stream.subscribe_node: buffer_size must be an integer")
		}
		if n > 0 && n <= maxStreamSubBufferSize {
			return int(n), nil
		}
	case float64:
		if math.Trunc(n) != n {
			return 0, errdefs.Validationf("stream.subscribe_node: buffer_size must be an integer")
		}
		if n > 0 && n <= maxStreamSubBufferSize {
			return int(n), nil
		}
	default:
		return 0, errdefs.Validationf("stream.subscribe_node: buffer_size must be a positive number, got %T", v)
	}
	return 0, errdefs.Validationf("stream.subscribe_node: buffer_size must be between 1 and %d", maxStreamSubBufferSize)
}

type streamNodeIterator struct {
	ctx    context.Context
	cancel context.CancelFunc
	sub    event.Subscription

	mu      sync.Mutex
	current map[string]any
	once    sync.Once

	targetNodeCount int
	terminalNodes   map[string]struct{}
}

// Next blocks until the next matching node stream event arrives. It returns false when
// the subscription or parent execution context is closed.
func (i *streamNodeIterator) Next() bool {
	return i.next(false, 0)
}

// NextTimeoutMS waits up to ms for the next matching node stream event. Timeout
// returns false without closing the subscription so scripts can fail open.
func (i *streamNodeIterator) NextTimeoutMS(raw any) (bool, error) {
	timeout, err := streamNextTimeout(raw)
	if err != nil {
		return false, err
	}
	return i.next(true, timeout), nil
}

func (i *streamNodeIterator) next(useTimeout bool, timeout time.Duration) bool {
	if i == nil || i.sub == nil {
		return false
	}
	if useTimeout && timeout == 0 {
		return i.poll()
	}
	var timer *time.Timer
	var timeoutC <-chan time.Time
	if useTimeout && timeout > 0 {
		timer = time.NewTimer(timeout)
		timeoutC = timer.C
		defer timer.Stop()
	}
	for {
		select {
		case env, ok := <-i.sub.C():
			if !ok {
				return false
			}
			if i.acceptEnvelope(env) {
				return true
			}
		case <-i.ctx.Done():
			return false
		case <-timeoutC:
			return false
		}
	}
}

func (i *streamNodeIterator) poll() bool {
	for {
		select {
		case env, ok := <-i.sub.C():
			if !ok {
				return false
			}
			if i.acceptEnvelope(env) {
				return true
			}
		case <-i.ctx.Done():
			return false
		default:
			return false
		}
	}
}

func (i *streamNodeIterator) acceptEnvelope(env event.Envelope) bool {
	cur, terminal, ok := streamNodeEnvelopeToMap(env)
	if !ok {
		return false
	}
	closeAfter := false
	i.mu.Lock()
	i.current = cur
	if terminal {
		if i.terminalNodes == nil {
			i.terminalNodes = make(map[string]struct{}, i.targetNodeCount)
		}
		i.terminalNodes[env.NodeID()] = struct{}{}
		closeAfter = len(i.terminalNodes) >= i.targetNodeCount
	}
	i.mu.Unlock()
	if closeAfter {
		_ = i.Close()
	}
	return true
}

func (i *streamNodeIterator) Current() map[string]any {
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

func (i *streamNodeIterator) Close() error {
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

const maxStreamNextTimeoutMS = int64(1<<63-1) / int64(time.Millisecond)

func streamNextTimeout(v any) (time.Duration, error) {
	ms, err := streamNonNegativeIntegerMS(v)
	if err != nil {
		return 0, err
	}
	if ms > maxStreamNextTimeoutMS {
		return 0, errdefs.Validationf("stream.subscribe_node.next_timeout_ms: ms is too large")
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func streamNonNegativeIntegerMS(v any) (int64, error) {
	switch n := v.(type) {
	case int:
		return streamSignedMS(int64(n))
	case int32:
		return streamSignedMS(int64(n))
	case int64:
		return streamSignedMS(n)
	case uint:
		return streamUnsignedMS(uint64(n))
	case uint32:
		return streamUnsignedMS(uint64(n))
	case uint64:
		return streamUnsignedMS(n)
	case float32:
		return streamFloatMS(float64(n))
	case float64:
		return streamFloatMS(n)
	default:
		return 0, errdefs.Validationf("stream.subscribe_node.next_timeout_ms: ms must be a non-negative integer, got %T", v)
	}
}

func streamSignedMS(n int64) (int64, error) {
	if n < 0 {
		return 0, errdefs.Validationf("stream.subscribe_node.next_timeout_ms: ms must be non-negative")
	}
	return n, nil
}

func streamUnsignedMS(n uint64) (int64, error) {
	if n > uint64(maxStreamNextTimeoutMS) {
		return 0, errdefs.Validationf("stream.subscribe_node.next_timeout_ms: ms is too large")
	}
	return int64(n), nil
}

func streamFloatMS(n float64) (int64, error) {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, errdefs.Validationf("stream.subscribe_node.next_timeout_ms: ms must be a finite number")
	}
	if math.Trunc(n) != n {
		return 0, errdefs.Validationf("stream.subscribe_node.next_timeout_ms: ms must be an integer")
	}
	if n < 0 {
		return 0, errdefs.Validationf("stream.subscribe_node.next_timeout_ms: ms must be non-negative")
	}
	if n > float64(maxStreamNextTimeoutMS) {
		return 0, errdefs.Validationf("stream.subscribe_node.next_timeout_ms: ms is too large")
	}
	return int64(n), nil
}

func streamNodeEnvelopeToMap(env event.Envelope) (map[string]any, bool, bool) {
	if engine.IsStreamDelta(env.Subject) {
		return streamDeltaEnvelopeToMap(env), false, true
	}

	str := string(env.Subject)
	if !strings.Contains(str, ".step.") {
		return nil, false, false
	}
	switch {
	case strings.HasSuffix(str, ".start"):
		return streamLifecycleEnvelopeToMap(env, "step.started", ""), false, true
	case strings.HasSuffix(str, ".complete"):
		return streamLifecycleEnvelopeToMap(env, "step.ended", "success"), true, true
	case strings.HasSuffix(str, ".error"):
		return streamLifecycleEnvelopeToMap(env, "step.ended", "error"), true, true
	case strings.HasSuffix(str, ".skipped"):
		return streamLifecycleEnvelopeToMap(env, "step.skipped", "skipped"), true, true
	}
	return nil, false, false
}

func streamDeltaEnvelopeToMap(env event.Envelope) map[string]any {
	out := decodeStreamDeltaPayloadMap(env)
	if out == nil {
		out = make(map[string]any, 8)
	}
	out["event"] = "stream.delta"

	addStreamEnvelopeMetadata(out, env)
	return out
}

func streamLifecycleEnvelopeToMap(env event.Envelope, eventName, status string) map[string]any {
	out := decodeEnvelopePayloadMap(env)
	if out == nil {
		out = make(map[string]any, 8)
	}
	out["event"] = eventName
	if status != "" {
		if _, ok := out["status"]; !ok {
			out["status"] = status
		}
	}

	addStreamEnvelopeMetadata(out, env)
	return out
}

func addStreamEnvelopeMetadata(out map[string]any, env event.Envelope) {
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

func decodeEnvelopePayloadMap(env event.Envelope) map[string]any {
	if len(env.Payload) == 0 {
		return nil
	}
	var asMap map[string]any
	if err := json.Unmarshal(env.Payload, &asMap); err == nil && asMap != nil {
		return asMap
	}
	var generic any
	if err := json.Unmarshal(env.Payload, &generic); err == nil {
		return map[string]any{"value": generic}
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
