package recall

import (
	ig "github.com/GizClaw/flowcraft/sdk/recall/internal/governance"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Governance bundles write-path policy hooks (docs §10.2).
type Governance = ig.Governance

// WritePolicy governs individual facts on the write path.
type WritePolicy = port.WritePolicy

// RetentionPolicy governs fact retention eligibility.
type RetentionPolicy = port.RetentionPolicy

// SensitivityPolicy governs sensitive content on the write path.
type SensitivityPolicy = port.SensitivityPolicy

// DefaultGovernance returns audit-only no-op policies.
func DefaultGovernance() Governance {
	return ig.Default()
}

// WithGovernance installs governance hooks on the write compiler.
// Custom WithCompiler configurations must wire Governance themselves.
func WithGovernance(g Governance) Option {
	return func(c *config) {
		c.governance = &g
	}
}
