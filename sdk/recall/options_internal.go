package recall

import (
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/compiler"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/evolution"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/fusion"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/governance"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/materialize"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/planner"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/queryintent"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/source"
	evidencestore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/evidence"
	temporalstore "github.com/GizClaw/flowcraft/sdk/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/telemetry"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

type config struct {
	store          temporalstore.Store
	evidenceStore  evidencestore.Store
	retrievalIndex retrieval.Index
	embedder       embedding.Embedder
	compiler       compiler.Compiler
	llmExtractor   *llmExtractorConfig
	resolver       compiler.ConflictResolver
	resolverSet    bool
	telemetry      telemetry.Hook

	extraProjections []projection.Projection

	queryCompiler queryintent.Compiler
	planner       planner.Planner
	sources       []source.CandidateSource
	fuser         fusion.Fuser
	materializer  materialize.Materializer
	fusionOpts    fusion.Options

	graphEnabled bool

	reranker Reranker

	governance *governance.Governance
	evolution  evolution.Runner
}

// llmExtractorConfig captures the args to compiler.NewLLMExtractor so New can
// defer default compiler wiring until all options have been applied.
type llmExtractorConfig struct {
	client llm.LLM
	tune   []LLMExtractorOption
}

func withTemporalStore(s temporalstore.Store) Option {
	return func(c *config) {
		if s != nil {
			c.store = s
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

func withConflictResolver(r compiler.ConflictResolver) Option {
	return func(c *config) {
		c.resolver = r
		c.resolverSet = true
	}
}

func withExtraProjection(p projection.Projection) Option {
	return func(c *config) {
		if p != nil {
			c.extraProjections = append(c.extraProjections, p)
		}
	}
}

func withQueryCompiler(qc queryintent.Compiler) Option {
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
