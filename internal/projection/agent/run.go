// Package agent contains projectors for agent run and trace events.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/projection/common"
)

// RunProjectorName is the canonical name for the run projector.
const RunProjectorName = "agent_run"

// RunState tracks the lifecycle state of an agent run per card.
type RunState struct {
	CardID      string
	RunID       string
	StartedAt   time.Time
	CompletedAt time.Time
	FailedAt    time.Time
	Error       string
	LastSeq     int64
}

// AgentRunProjector tracks agent run lifecycle events per card.
// It uses RestoreReplay mode because run events are relatively rare and
// we want to preserve the full history.
type AgentRunProjector struct {
	log  eventlog.Log
	mu   sync.RWMutex
	runs map[string]*RunState // cardID → state
}

var _ projection.Projector = (*AgentRunProjector)(nil)

// NewAgentRunProjector constructs an AgentRunProjector.
func NewAgentRunProjector(log eventlog.Log) *AgentRunProjector {
	return &AgentRunProjector{
		log:  log,
		runs: make(map[string]*RunState),
	}
}

func (p *AgentRunProjector) Name() string { return RunProjectorName }
func (p *AgentRunProjector) Subscribes() []string {
	return []string{"agent.run.started", "agent.run.completed", "agent.run.failed"}
}
func (p *AgentRunProjector) RestoreMode() projection.RestoreMode { return projection.RestoreReplay }
func (p *AgentRunProjector) OnReady(context.Context) error       { return nil }

// AgentRunProjector uses snapshots for efficient restart.
func (p *AgentRunProjector) SnapshotFormatVersion() int { return 1 }
func (p *AgentRunProjector) SnapshotEvery() (int64, time.Duration) {
	return projection.DefaultSnapshotEveryEvents, projection.DefaultSnapshotEveryPeriod
}

// Snapshot returns the current cursor and encoded snapshot.
func (p *AgentRunProjector) Snapshot(ctx context.Context) (int64, []byte, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	data := make(map[string]*RunState, len(p.runs))
	for k, v := range p.runs {
		data[k] = v
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return 0, nil, fmt.Errorf("agent_run snapshot: marshal: %w", err)
	}
	cp, err := p.log.Checkpoints().Get(ctx, RunProjectorName)
	if err != nil {
		return 0, nil, err
	}
	return cp, payload, nil
}

// LoadSnapshot restores projector state from a previously saved snapshot.
func (p *AgentRunProjector) LoadSnapshot(_ context.Context, _ int64, payload []byte) error {
	if payload == nil {
		return nil
	}
	data := make(map[string]*RunState)
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("agent_run load snapshot: unmarshal: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs = data
	return nil
}

// Apply dispatches based on event type.
func (p *AgentRunProjector) Apply(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	switch env.Type {
	case "agent.run.started":
		return p.handleRunStarted(ctx, uow, env)
	case "agent.run.completed":
		return p.handleRunCompleted(ctx, uow, env)
	case "agent.run.failed":
		return p.handleRunFailed(ctx, uow, env)
	}
	return nil
}

func (p *AgentRunProjector) handleRunStarted(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.AgentRunStartedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs[payload.CardID] = &RunState{
		CardID:    payload.CardID,
		RunID:     payload.RunID,
		StartedAt: parseTs(env.Ts),
		LastSeq:   env.Seq,
	}
	return nil
}

func (p *AgentRunProjector) handleRunCompleted(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.AgentRunCompletedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if run, ok := p.runs[payload.CardID]; ok {
		run.CompletedAt = parseTs(env.Ts)
		run.LastSeq = env.Seq
	}
	return nil
}

func (p *AgentRunProjector) handleRunFailed(ctx context.Context, uow eventlog.UnitOfWork, env eventlog.Envelope) error {
	var payload eventlog.AgentRunFailedPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if run, ok := p.runs[payload.CardID]; ok {
		run.FailedAt = parseTs(env.Ts)
		run.Error = payload.Error
		run.LastSeq = env.Seq
	}
	return nil
}

// GetRun returns the run state for a card (nil if not found).
func (p *AgentRunProjector) GetRun(cardID string) *RunState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.runs[cardID]
}
