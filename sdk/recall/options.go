package recall

import (
	"github.com/GizClaw/flowcraft/sdk/recall/internal/compiler"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/fusion"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/materialize"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/source"
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

	// Read-path overrides. nil means "use the default wiring".
	planner      planner.Planner
	sources      []source.CandidateSource
	fuser        fusion.Fuser
	materializer materialize.Materializer
	fusionOpts   fusion.Options
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

// WithPlanner overrides the read-path planner. The default is a
// deterministic rule-based planner.
func WithPlanner(p planner.Planner) Option {
	return func(c *config) {
		if p != nil {
			c.planner = p
		}
	}
}

// WithSources overrides the candidate source set. When non-empty,
// the default retrieval+entity sources are NOT registered; callers
// are responsible for wiring whatever sources they need (including
// re-adding the defaults).
func WithSources(sources ...source.CandidateSource) Option {
	return func(c *config) {
		for _, s := range sources {
			if s != nil {
				c.sources = append(c.sources, s)
			}
		}
	}
}

// WithFuser overrides the fusion algorithm. The default is
// fusion.WeightedRRF.
func WithFuser(f fusion.Fuser) Option {
	return func(c *config) {
		if f != nil {
			c.fuser = f
		}
	}
}

// WithMaterializer overrides the materializer. The default uses
// the configured TemporalFactStore.
func WithMaterializer(m materialize.Materializer) Option {
	return func(c *config) {
		if m != nil {
			c.materializer = m
		}
	}
}

// WithFusionOptions overrides default fusion options. Per-source
// weights default to retrieval=1.0, entity=0.8 when left zero.
func WithFusionOptions(opts fusion.Options) Option {
	return func(c *config) {
		c.fusionOpts = opts
	}
}
