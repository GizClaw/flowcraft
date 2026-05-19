package recall

import (
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/compiler"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/fusion"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/materialize"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/source"
	evidencestore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/evidence"
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
	evidenceStore  evidencestore.Store
	retrievalIndex retrieval.Index
	compiler       compiler.Compiler
	llmExtractor   *llmExtractorConfig
	resolver       compiler.ConflictResolver
	resolverSet    bool
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

// WithEvidenceStore wires a secondary lookup store for evidence
// attached to canonical facts (docs §7.2). The store is OPTIONAL:
// embedded TemporalFact.EvidenceRefs / EvidenceText /
// SourceMessageIDs stay authoritative and rebuildable. When
// configured:
//
//   - Save mirror-appends evidence after store.Append succeeds.
//     A mirror-append failure is treated as Required and rolls
//     back the canonical write so the store and the lookup
//     adapter never diverge from each other.
//   - Forget best-effort sweeps the evidence index after
//     store.Delete succeeds; failures surface via telemetry only
//     because the canonical evidence has already gone with the
//     fact.
//   - RebuildAll re-appends evidence from the canonical snapshot
//     so the adapter can be rebuilt without consulting any
//     external state.
//
// Passing nil disables the secondary store; Memory.GetEvidence
// then falls back to TemporalFact.EvidenceRefs.
func WithEvidenceStore(s evidencestore.Store) Option {
	return func(c *config) {
		c.evidenceStore = s
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
//
// Passing WithCompiler replaces the whole compiler pipeline; if you
// only want to enable LLM extraction on top of the default stages,
// use WithLLMExtractor instead.
func WithCompiler(cp compiler.Compiler) Option {
	return func(c *config) {
		if cp != nil {
			c.compiler = cp
		}
	}
}

// llmExtractorConfig captures the args to compiler.NewLLMExtractor
// so we can defer the wiring until New() decides whether to build
// the default compiler.
type llmExtractorConfig struct {
	client llm.LLM
	tune   []LLMExtractorOption
}

// LLMExtractorOption configures the LLM extractor wired by
// WithLLMExtractor. Tunable knobs live behind small option types so
// the facade doesn't grow positional arguments as quality features
// land in later phases.
type LLMExtractorOption func(*compiler.LLMExtractor)

// WithLLMExtractorSystemPrompt overrides the default system prompt.
func WithLLMExtractorSystemPrompt(prompt string) LLMExtractorOption {
	return func(e *compiler.LLMExtractor) {
		if prompt != "" {
			e.System = prompt
		}
	}
}

// WithLLMExtractorTemperature sets the sampling temperature. Zero
// means "use provider default".
func WithLLMExtractorTemperature(t float64) LLMExtractorOption {
	return func(e *compiler.LLMExtractor) { e.Temperature = t }
}

// WithLLMExtractorSchemaName labels the JSON schema for structured
// output (some providers display this in their dashboards / logs).
func WithLLMExtractorSchemaName(name string) LLMExtractorOption {
	return func(e *compiler.LLMExtractor) {
		if name != "" {
			e.SchemaName = name
		}
	}
}

// WithLLMExtractorExtraOptions forwards provider-specific
// llm.GenerateOption values on every extraction call (e.g. provider
// extra params, reasoning toggles).
func WithLLMExtractorExtraOptions(opts ...llm.GenerateOption) LLMExtractorOption {
	return func(e *compiler.LLMExtractor) {
		e.ExtraOptions = append(e.ExtraOptions, opts...)
	}
}

// WithLLMExtractor enables LLM-driven fact extraction in the
// default compiler pipeline. The supplied client is consulted on
// Save calls whose SaveRequest carries a non-empty Input.Text path.
//
// Interaction with WithCompiler:
//   - If only WithLLMExtractor is passed, New constructs the
//     default compiler stages and substitutes the LLM extractor.
//   - If WithCompiler is also passed, the caller-supplied compiler
//     wins and WithLLMExtractor is ignored (the caller is wiring
//     stages manually anyway).
//
// nil client falls back to the deterministic passthrough extractor.
func WithLLMExtractor(client llm.LLM, opts ...LLMExtractorOption) Option {
	return func(c *config) {
		if client == nil {
			return
		}
		c.llmExtractor = &llmExtractorConfig{client: client, tune: opts}
	}
}

// WithConflictResolver overrides the conflict resolver consulted
// between compile and store.Append. Defaults to compiler.NewResolver().
// Pass a nil-checked resolver to disable supersede behaviour
// entirely (treat every compiled fact as a fresh append).
func WithConflictResolver(r compiler.ConflictResolver) Option {
	return func(c *config) {
		c.resolver = r
		c.resolverSet = true
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
