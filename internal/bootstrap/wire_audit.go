package bootstrap

import (
	"fmt"

	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/projection/audit"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// wireAuditProjector builds + registers the AuditProjector. Audit has no
// upstream dependencies; it consumes every envelope so it sits at the root
// of the projector dependency graph.
func wireAuditProjector(c *ProjectorComponents, mgr *projection.Manager, log eventlog.Log) error {
	c.Audit = audit.NewAuditProjector(log)
	if err := mgr.RegisterProjector(c.Audit, nil); err != nil {
		return fmt.Errorf("register audit projector: %w", err)
	}
	return nil
}
