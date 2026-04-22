package bootstrap

import (
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
)

// WireProjectionManager constructs the per-process ProjectorManager. R3
// registers concrete projectors against this manager.
func WireProjectionManager(cfg projection.ManagerConfig) *projection.Manager {
	return projection.NewManager(cfg)
}
