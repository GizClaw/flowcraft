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
// The defaults supplied by New build a fully in-memory v2 stack. The
// public options expose stable integration points only; lower-level
// store/compiler/source/projection injection remains package-internal
// until those contracts are ready to support external implementations.
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
	queryCompiler compiler.QueryCompiler
	planner       planner.Planner
	sources       []source.CandidateSource
	fuser         fusion.Fuser
	materializer  materialize.Materializer
	fusionOpts    fusion.Options

	// graphEnabled wires the optional EntityGraph projection and
	// graph source (docs §17: default off).
	graphEnabled bool
}

func withTemporalStore(s temporalstore.Store) Option {
	return func(c *config) {
		if s != nil {
			c.store = s
		}
	}
}

func withEvidenceStore(s evidencestore.Store) Option {
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

func withCompiler(cp compiler.Compiler) Option {
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
// WithLLMExtractor. It is intentionally opaque so the public facade
// does not expose internal compiler types.
type LLMExtractorOption struct {
	apply func(*compiler.LLMExtractor)
}

func newLLMExtractorOption(apply func(*compiler.LLMExtractor)) LLMExtractorOption {
	return LLMExtractorOption{apply: apply}
}

// WithLLMExtractorSystemPrompt overrides the default system prompt.
func WithLLMExtractorSystemPrompt(prompt string) LLMExtractorOption {
	return newLLMExtractorOption(func(e *compiler.LLMExtractor) {
		if prompt != "" {
			e.System = prompt
		}
	})
}

// WithLLMExtractorTemperature sets the sampling temperature. Zero
// means "use provider default".
func WithLLMExtractorTemperature(t float64) LLMExtractorOption {
	return newLLMExtractorOption(func(e *compiler.LLMExtractor) { e.Temperature = t })
}

// WithLLMExtractorSchemaName labels the JSON schema for structured
// output (some providers display this in their dashboards / logs).
func WithLLMExtractorSchemaName(name string) LLMExtractorOption {
	return newLLMExtractorOption(func(e *compiler.LLMExtractor) {
		if name != "" {
			e.SchemaName = name
		}
	})
}

// WithLLMExtractorExtraOptions forwards provider-specific
// llm.GenerateOption values on every extraction call (e.g. provider
// extra params, reasoning toggles).
func WithLLMExtractorExtraOptions(opts ...llm.GenerateOption) LLMExtractorOption {
	return newLLMExtractorOption(func(e *compiler.LLMExtractor) {
		e.ExtraOptions = append(e.ExtraOptions, opts...)
	})
}

// WithLLMExtractor enables LLM-driven fact extraction in the
// default compiler pipeline. The supplied client is consulted on
// Save calls whose SaveRequest carries a non-empty Input.Text path.
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

func withConflictResolver(r compiler.ConflictResolver) Option {
	return func(c *config) {
		c.resolver = r
		c.resolverSet = true
	}
}

// WithTelemetryHook installs a telemetry observer for projection
// fanout. Defaults to a no-op hook.
func WithTelemetryHook(hook TelemetryHook) Option {
	return func(c *config) {
		if hook != nil {
			c.telemetry = hook
		}
	}
}

func withExtraProjection(p projection.Projection) Option {
	return func(c *config) {
		if p != nil {
			c.extraProjections = append(c.extraProjections, p)
		}
	}
}

func withQueryCompiler(qc compiler.QueryCompiler) Option {
	return func(c *config) {
		if qc != nil {
			c.queryCompiler = qc
		}
	}
}

func withPlanner(p planner.Planner) Option {
	return func(c *config) {
		if p != nil {
			c.planner = p
		}
	}
}

func withSources(sources ...source.CandidateSource) Option {
	return func(c *config) {
		for _, s := range sources {
			if s != nil {
				c.sources = append(c.sources, s)
			}
		}
	}
}

func withFuser(f fusion.Fuser) Option {
	return func(c *config) {
		if f != nil {
			c.fuser = f
		}
	}
}

func withMaterializer(m materialize.Materializer) Option {
	return func(c *config) {
		if m != nil {
			c.materializer = m
		}
	}
}

func withFusionOptions(opts fusion.Options) Option {
	return func(c *config) {
		c.fusionOpts = opts
	}
}

// WithGraphEnabled opts into the EntityGraph projection and graph
// source. Graph expansion is off by default (docs §17).
func WithGraphEnabled(enabled bool) Option {
	return func(c *config) {
		c.graphEnabled = enabled
	}
}
