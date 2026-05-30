package ops

import "github.com/GizClaw/flowcraft/memory/recall"

// LocalReadinessOptions is a lenient preset for tests, demos, and single-node
// local deployments where small backlogs are expected during bursts.
func LocalReadinessOptions() recall.ReadinessOptions {
	return recall.ReadinessOptions{
		MaxSideEffectBacklog:    100,
		MaxAsyncSemanticBacklog: 50,
		MaxExpiredLeases:        1,
		MaxDeadLetters:          0,
	}
}

// ProductionReadinessOptions is a conservative baseline for dashboards. Teams
// should tune backlog ceilings to their traffic shape, but dead letters and
// expired leases default to strict handling because they usually need action.
func ProductionReadinessOptions() recall.ReadinessOptions {
	return recall.ReadinessOptions{
		RequireAsyncSemantic:    true,
		MaxSideEffectBacklog:    1000,
		MaxAsyncSemanticBacklog: 500,
		MaxExpiredLeases:        0,
		MaxDeadLetters:          0,
	}
}

// WithLocalReadiness installs LocalReadinessOptions.
func WithLocalReadiness() Option {
	return WithReadinessOptions(LocalReadinessOptions())
}

// WithProductionReadiness installs ProductionReadinessOptions.
func WithProductionReadiness() Option {
	return WithReadinessOptions(ProductionReadinessOptions())
}
