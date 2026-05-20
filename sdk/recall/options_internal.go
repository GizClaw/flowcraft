package recall

import (
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/governance"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

type config struct {
	store          port.TemporalStore
	evidenceStore  port.EvidenceStore
	retrievalIndex retrieval.Index
	embedder       embedding.Embedder
	compiler       port.Ingestor
	llmExtractor   *llmExtractorConfig
	resolver       port.ConflictResolver
	resolverSet    bool
	telemetry      port.TelemetryHook

	extraProjections []port.Projection

	queryCompiler port.IntentCompiler
	planner       port.Planner
	sources       []port.Source
	fuser         port.Fuser
	materializer  port.Materializer
	fusionOpts    port.FusionOptions

	graphEnabled bool

	reranker Reranker

	governance *governance.Governance
	evolution  port.EvolutionRunner
}

// llmExtractorConfig captures the args to ingest.NewLLMExtractor so New can
// defer default compiler wiring until all options have been applied.
type llmExtractorConfig struct {
	client llm.LLM
	tune   []LLMExtractorOption
}

func withTemporalStore(s port.TemporalStore) Option {
	return func(c *config) {
		if s != nil {
			c.store = s
		}
	}
}

func withCompiler(cp port.Ingestor) Option {
	return func(c *config) {
		if cp != nil {
			c.compiler = cp
		}
	}
}

func withConflictResolver(r port.ConflictResolver) Option {
	return func(c *config) {
		c.resolver = r
		c.resolverSet = true
	}
}

func withExtraProjection(p port.Projection) Option {
	return func(c *config) {
		if p != nil {
			c.extraProjections = append(c.extraProjections, p)
		}
	}
}

func withQueryCompiler(qc port.IntentCompiler) Option {
	return func(c *config) {
		if qc != nil {
			c.queryCompiler = qc
		}
	}
}

func withPlanner(p port.Planner) Option {
	return func(c *config) {
		if p != nil {
			c.planner = p
		}
	}
}

func withSources(sources ...port.Source) Option {
	return func(c *config) {
		for _, s := range sources {
			if s != nil {
				c.sources = append(c.sources, s)
			}
		}
	}
}

func withFuser(f port.Fuser) Option {
	return func(c *config) {
		if f != nil {
			c.fuser = f
		}
	}
}

func withMaterializer(m port.Materializer) Option {
	return func(c *config) {
		if m != nil {
			c.materializer = m
		}
	}
}

func withFusionOptions(opts port.FusionOptions) Option {
	return func(c *config) {
		c.fusionOpts = opts
	}
}
