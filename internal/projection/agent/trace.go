// Package agent contains projectors for agent run and trace events.
package agent

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// TraceProjectorName is the canonical name for the trace projector.
const TraceProjectorName = "agent_trace"

// DeltaTailSize is the max number of delta snippets kept in memory per card.
const DeltaTailSize = 200

// ToolInvocation records a single tool call and its result.
type ToolInvocation struct {
	CallID   string
	ToolName string
	Args     string
	Output   string
	Error    string
	DurationMs int64
	Seq      int64
}

// Trace tracks the in-memory trace for a single card run.
type Trace struct {
	CardID       string
	RunID        string
	Tools        []ToolInvocation
	DeltaTail    []DeltaSnippet // last N deltas
	DeltaSeq     int64
	LastUpdated  time.Time
}

// DeltaSnippet is a short delta for display.
type DeltaSnippet struct {
	Content   string
	Role      string
	Finished  bool
	Seq       int64
}

// AgentTraceProjector maintains in-memory agent trace read-models with a 24h window.
type AgentTraceProjector struct {
	log   eventlog.Log
	mu    sync.RWMutex
	traces map[string]*Trace // cardID → trace
}

var _ projection.Projector = (*AgentTraceProjector)(nil)

// NewAgentTraceProjector constructs an AgentTraceProjector.
func NewAgentTraceProjector(log eventlog.Log) *AgentTraceProjector {
	return &AgentTraceProjector{
		log:    log,
		traces: make(map[string]*Trace),
	}
}

func (p *AgentTraceProjector) Name() string                         { return TraceProjectorName }
func (p *AgentTraceProjector) Subscribes() []string {
	return []string{
		"agent.stream.delta",
		"agent.thinking.delta",
		"agent.tool.invoked",
		"agent.tool.returned",
	}
}
func (p *AgentTraceProjector) RestoreMode() projection.RestoreMode { return projection.RestoreWindow }
func (p *AgentTraceProjector) WindowSize() time.Duration        { return 24 * time.Hour }
func (p *AgentTraceProjector) OnReady(context.Context) error   { return nil }

// AgentTraceProjector does not use snapshots.
func (p *AgentTraceProjector) SnapshotFormatVersion() int { return 0 }
func (p *AgentTraceProjector) SnapshotEvery() (int64, time.Duration) {
	return 0, 0
}
func (p *AgentTraceProjector) Snapshot(context.Context) (int64, []byte, error) { return 0, nil, nil }
func (p *AgentTraceProjector) LoadSnapshot(context.Context, int64, []byte) error { return nil }

// Apply dispatches based on event type.
func (p *AgentTraceProjector) Apply(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	switch env.Type {
	case "agent.stream.delta":
		return p.handleStreamDelta(ctx, uow, env)
	case "agent.thinking.delta":
		return p.handleThinkingDelta(ctx, uow, env)
	case "agent.tool.invoked":
		return p.handleToolInvoked(ctx, uow, env)
	case "agent.tool.returned":
		return p.handleToolReturned(ctx, uow, env)
	}
	return nil
}

func (p *AgentTraceProjector) handleStreamDelta(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.AgentStreamDeltaPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	trace := p.getOrCreateTrace(payload.CardID, payload.RunID)
	trace.DeltaSeq++
	finished := false
	// Check if finished field exists in payload (via raw parse)
	var raw struct {
		Finished bool `json:"finished"`
	}
	json.Unmarshal(env.Payload, &raw)
	finished = raw.Finished
	tail := append(trace.DeltaTail, DeltaSnippet{
		Content:  payload.Delta,
		Seq:     trace.DeltaSeq,
		Finished: finished,
	})
	if len(tail) > DeltaTailSize {
		tail = tail[len(tail)-DeltaTailSize:]
	}
	trace.DeltaTail = tail
	trace.LastUpdated = parseTs(env.Ts)
	return nil
}

func (p *AgentTraceProjector) handleThinkingDelta(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.AgentThinkingDeltaPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	trace := p.getOrCreateTrace(payload.CardID, payload.RunID)
	trace.DeltaSeq++
	// thinking delta stored with role "assistant" for display purposes
	tail := append(trace.DeltaTail, DeltaSnippet{
		Content: payload.Delta,
		Role:   "assistant",
		Seq:    trace.DeltaSeq,
	})
	if len(tail) > DeltaTailSize {
		tail = tail[len(tail)-DeltaTailSize:]
	}
	trace.DeltaTail = tail
	trace.LastUpdated = parseTs(env.Ts)
	return nil
}

func (p *AgentTraceProjector) handleToolInvoked(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.AgentToolInvokedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	trace := p.getOrCreateTrace(payload.CardID, payload.RunID)
	trace.Tools = append(trace.Tools, ToolInvocation{
		CallID:   payload.CallID,
		ToolName: payload.ToolName,
		Seq:      env.Seq,
	})
	trace.LastUpdated = parseTs(env.Ts)
	return nil
}

func (p *AgentTraceProjector) handleToolReturned(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.AgentToolReturnedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	trace := p.getOrCreateTrace(payload.CardID, payload.RunID)
	// Find the matching invocation and update it
	for i := len(trace.Tools) - 1; i >= 0; i-- {
		if trace.Tools[i].CallID == payload.CallID {
			trace.Tools[i].Output = payload.Output
			trace.Tools[i].Error = payload.Error
			break
		}
	}
	trace.LastUpdated = parseTs(env.Ts)
	return nil
}

func (p *AgentTraceProjector) getOrCreateTrace(cardID, runID string) *Trace {
	if t, ok := p.traces[cardID]; ok && t.RunID == runID {
		return t
	}
	t := &Trace{CardID: cardID, RunID: runID}
	p.traces[cardID] = t
	return t
}

// GetTrace returns the trace for a card (nil if not found).
func (p *AgentTraceProjector) GetTrace(cardID string) *Trace {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.traces[cardID]
}
