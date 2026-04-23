package bootstrap

import (
	"fmt"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	projectionagent "github.com/GizClaw/flowcraft/internal/projection/agent"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// wireAgentProjectors registers the agent-domain projectors:
//   - AgentRunProjector tracks per-card run lifecycle and uses RestoreSnapshot
//     so it doesn't replay every agent.* event from beginning on restart.
//   - AgentTraceProjector keeps the most recent trace window per card via
//     RestoreWindow (no snapshot store needed).
func wireAgentProjectors(c *ProjectorComponents, mgr *projection.Manager, log eventlog.Log, snapshots projection.SnapshotStore) error {
	c.AgentRun = projectionagent.NewAgentRunProjector(log)
	if err := mgr.RegisterProjector(c.AgentRun, nil, projection.WithSnapshotStore(snapshots)); err != nil {
		return fmt.Errorf("register agent_run projector: %w", err)
	}
	c.AgentTrace = projectionagent.NewAgentTraceProjector(log)
	if err := mgr.RegisterProjector(c.AgentTrace, nil); err != nil {
		return fmt.Errorf("register agent_trace projector: %w", err)
	}
	return nil
}
