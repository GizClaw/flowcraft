package recall

import "github.com/GizClaw/flowcraft/sdk/recall/internal/port"

// EvolutionRunner observes completed Save/Recall calls (docs §10.1).
// Errors are telemetry-only and must not fail Save/Recall.
type EvolutionRunner = port.EvolutionRunner

// WithEvolution installs a background evolution runner. The default
// is a no-op that is not invoked.
func WithEvolution(r EvolutionRunner) Option {
	return func(c *config) {
		if r != nil {
			c.evolution = r
		}
	}
}
