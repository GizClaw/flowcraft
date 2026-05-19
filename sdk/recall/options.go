package recall

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/compiler"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// Option configures Memory at construction time.
//
// The defaults supplied by New build a fully in-memory v2 stack:
// in-memory temporal store, deterministic compiler, in-memory
// retrieval projection and entity projection. Callers typically only
// override pieces in tests or when wiring a durable backend.
type Option func(*config)

type config struct {
	store          temporalstore.Store
	retrievalIndex retrieval.Index
	compiler       compiler.Compiler
	telemetry      projection.TelemetryHook

	// extraProjections are appended to the canonical projection set.
	// They are typically Optional and provide forward room for
	// timeline / relation / profile to plug in later phases.
	extraProjections []projection.Projection
}

// WithTemporalStore overrides the canonical TemporalFactStore. Use
// in tests or to plug in a durable backend; the default is the
// in-memory store from internal/store/temporal.
func WithTemporalStore(s temporalstore.Store) Option {
	return func(c *config) {
		if s != nil {
			c.store = s
		}
	}
}

// WithRetrievalIndex overrides the retrieval backend that powers the
// retrieval projection. The default is sdk/retrieval/memory.New().
func WithRetrievalIndex(idx retrieval.Index) Option {
	return func(c *config) {
		if idx != nil {
			c.retrievalIndex = idx
		}
	}
}

// WithCompiler overrides the write-time compiler. The default is
// compiler.Default() with deterministic Phase 1 stages.
func WithCompiler(cp compiler.Compiler) Option {
	return func(c *config) {
		if cp != nil {
			c.compiler = cp
		}
	}
}

// WithTelemetryHook installs a telemetry observer for projection
// fanout. Defaults to projection.NopTelemetry.
func WithTelemetryHook(hook projection.TelemetryHook) Option {
	return func(c *config) {
		if hook != nil {
			c.telemetry = hook
		}
	}
}

// WithExtraProjection registers an additional projection. Required vs
// Optional is taken from the projection itself. Reserved for callers
// that bring their own profile / timeline / relation views ahead of
// Phase 6.
func WithExtraProjection(p projection.Projection) Option {
	return func(c *config) {
		if p != nil {
			c.extraProjections = append(c.extraProjections, p)
		}
	}
}
