package recall

import (
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/governance"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/ranker"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/asyncsemantic"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/sideeffect"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/text/timex"
)

// Option configures Memory at construction time.
//
// The defaults supplied by New build a fully in-memory v2 stack. The
// public options expose stable integration points only; lower-level
// store/compiler/source/projection injection remains package-internal
// until those contracts are ready to support external implementations.
type Option func(*config)

// EntityExtractor mines entity mentions from a fact's natural-language
// content during Save. The default implementation is deterministic and
// English-centric; production callers can provide an LLM-, NER-, or
// tokenizer-backed extractor for multilingual content.
type EntityExtractor interface {
	ExtractEntities(content string, known []EntitySnapshot) []string
}

// config aggregates every option-supplied piece of the Memory stack.
// Public With* helpers populate the canonical fields; internal with*
// helpers (used by tests and adapter packages) inject lower-level
// contracts that are not part of the stable public surface yet.
type config struct {
	store           port.TemporalStore
	evidenceStore   port.EvidenceStore
	retrievalIndex  retrieval.Index
	embedder        embedding.Embedder
	compiler        port.Ingestor
	llmExtractor    *llmExtractorConfig
	timeParser      timex.Parser
	entityExtractor port.EntityExtractor
	resolver        port.ConflictResolver
	resolverSet     bool
	telemetry       port.TelemetryHook

	extraProjections []port.Projection

	queryCompiler port.IntentCompiler
	planner       port.Planner
	sources       []port.Source
	fuser         port.Fuser
	materializer  port.Materializer
	fusionOpts    port.FusionOptions

	graphEnabled bool

	reranker      port.Reranker
	contextRanker port.Ranker

	governance *governance.Governance
	evolution  port.EvolutionRunner

	asyncSemanticQueue port.AsyncSemanticQueue
	sideEffectOutbox   port.SideEffectOutbox
}

// llmExtractorConfig captures the args to ingest.NewLLMExtractor so New can
// defer default compiler wiring until all options have been applied.
type llmExtractorConfig struct {
	client llm.LLM
	tune   []LLMExtractorOption
}

// Internal injection helpers — used by package-internal tests and
// adapter wiring. Not part of the public surface.

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

// WithEvidenceStore installs an optional secondary evidence lookup
// adapter. Save keeps embedded EvidenceRefs authoritative; adapter
// write failures are telemetry-only and RebuildAll can rehydrate the
// adapter from canonical facts.
func WithEvidenceStore(s EvidenceStore) Option {
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

// WithEmbedder enables hybrid lexical+semantic retrieval. When set, the
// retrieval projection embeds every indexed fact's searchable content
// into Doc.Vector and the retrieval source embeds each query into
// SearchRequest.QueryVector, so the in-memory index (and any
// embedder-aware backend) can score documents by cosine similarity
// alongside BM25. Defaults to nil (BM25-only). The embedder is
// optional throughout; if Embed/EmbedBatch fails for a fact the
// projection falls back to BM25 indexing for that fact rather than
// failing the entire Save.
func WithEmbedder(e embedding.Embedder) Option {
	return func(c *config) {
		c.embedder = e
	}
}

// LLMExtractorOption configures the LLM extractor wired by
// WithLLMExtractor. It is intentionally opaque so the public facade
// does not expose internal compiler types.
type LLMExtractorOption struct {
	apply func(*ingest.LLMExtractor)
}

func newLLMExtractorOption(apply func(*ingest.LLMExtractor)) LLMExtractorOption {
	return LLMExtractorOption{apply: apply}
}

// WithLLMExtractorSystemPrompt overrides the default system prompt.
func WithLLMExtractorSystemPrompt(prompt string) LLMExtractorOption {
	return newLLMExtractorOption(func(e *ingest.LLMExtractor) {
		if prompt != "" {
			e.System = prompt
		}
	})
}

// WithLLMExtractorTemperature sets the sampling temperature. Zero
// means "use provider default".
func WithLLMExtractorTemperature(t float64) LLMExtractorOption {
	return newLLMExtractorOption(func(e *ingest.LLMExtractor) { e.Temperature = t })
}

// WithLLMExtractorSchemaName labels the JSON schema for structured
// output (some providers display this in their dashboards / logs).
func WithLLMExtractorSchemaName(name string) LLMExtractorOption {
	return newLLMExtractorOption(func(e *ingest.LLMExtractor) {
		if name != "" {
			e.SchemaName = name
		}
	})
}

// WithLLMExtractorExtraOptions forwards provider-specific
// llm.GenerateOption values on every extraction call (e.g. provider
// extra params, reasoning toggles).
func WithLLMExtractorExtraOptions(opts ...llm.GenerateOption) LLMExtractorOption {
	return newLLMExtractorOption(func(e *ingest.LLMExtractor) {
		e.ExtraOptions = append(e.ExtraOptions, opts...)
	})
}

// WithLLMExtractor enables LLM-driven fact extraction in the
// default compiler pipeline. The supplied client is consulted on
// Save calls whose SaveRequest carries non-empty Turns; the
// extractor renders Turns into a canonical JSONL wire shape and
// asks the model to emit a minimal {text, evidence_refs} payload.
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

// WithTimeParser installs the natural-language time parser used by the
// default Structurizer during Save. Parsed timestamps are carried to the
// TimeResolver as canonical metadata, so multilingual parsers can resolve
// expressions such as "四年前" without teaching the resolver every language.
//
// Nil keeps the SDK default parser.
func WithTimeParser(parser timex.Parser) Option {
	return func(c *config) {
		if parser != nil {
			c.timeParser = parser
		}
	}
}

// WithEntityExtractor installs the entity extractor used by the default
// Structurizer during Save. Nil keeps the deterministic rule-based default.
func WithEntityExtractor(extractor EntityExtractor) Option {
	return func(c *config) {
		if extractor != nil {
			c.entityExtractor = extractor
		}
	}
}

// WithTelemetryHook installs a telemetry observer for projection fanout,
// drift detection, and high-level Save/Recall pipeline stages.
func WithTelemetryHook(hook TelemetryHook) Option {
	return func(c *config) {
		if hook != nil {
			c.telemetry = hook
		}
	}
}

// WithGraphEnabled opts into the EntityGraph projection and graph
// source. Graph expansion is off by default (docs §17).
func WithGraphEnabled(enabled bool) Option {
	return func(c *config) {
		c.graphEnabled = enabled
	}
}

// WithReranker installs a Reranker into the Recall pipeline. The
// reranker fires between rank-boost and the final TotalCap, so it
// sees up to fusionCandidateCap(TotalCap) hits (default = 2 ×
// requested topK).
//
// Set to nil to keep the default fusion-only ranking. The function
// is a no-op for nil so callers can wire it conditionally from CLI
// flags without an extra branch.
func WithReranker(r Reranker) Option {
	return func(c *config) {
		if r == nil {
			return
		}
		c.reranker = r
	}
}

// WithAsyncSemanticQueue installs the durable outbox backend that
// Save(WriteModeAsyncSemantic) writes to inside the scope write lock.
//
// When this option is unset, SaveRequest.Mode == WriteModeAsyncSemantic
// returns errdefs.Validation before performing any side effect — the
// SDK refuses to silently degrade to sync because callers configure
// WriteModeAsyncSemantic precisely to decouple their latency budget
// from LLM extraction.
//
// SLA: see internal-docs/recall-v2-async-semantic-write.md §4.2 —
// Enqueue MUST complete < 10ms p99 for local backends; remote
// backends MUST expose an outbox facade here and drain to the remote
// service in a backend-internal worker, outside the scope write lock,
// so scope throughput does not regress with queue backend latency.
func WithAsyncSemanticQueue(q AsyncSemanticQueue) Option {
	return func(c *config) {
		if q != nil {
			c.asyncSemanticQueue = q
		}
	}
}

// NewInMemoryAsyncSemanticQueue returns the process-local durable
// outbox used by tests and local development. Jobs are lost on
// process restart; production callers supply a remote backend via
// WithAsyncSemanticQueue.
func NewInMemoryAsyncSemanticQueue() AsyncSemanticQueue {
	return asyncsemantic.New()
}

// WithSideEffectOutbox installs the durable outbox for commit-after
// projection / evolution / embedding work. When unset, New wires the
// in-memory implementation automatically.
func WithSideEffectOutbox(q SideEffectOutbox) Option {
	return func(c *config) {
		if q != nil {
			c.sideEffectOutbox = q
		}
	}
}

// NewInMemorySideEffectOutbox returns the process-local outbox used
// by tests and the default Memory stack.
func NewInMemorySideEffectOutbox() SideEffectOutbox {
	return sideeffect.New()
}

// NewLLMReranker returns a Reranker backed by an llm.LLM client.
// The reranker uses the canonical recall-tuned prompt and JSON
// schema; it degrades to a no-op when the supplied client is nil so
// CLI flags can wire it conditionally without an extra branch.
//
// Tuning (MaxBatch / SnippetMax / Prompt / ExtraOptions) lives on
// the internal/ranker.LLMReranker provider and is accessed via the
// type-asserted return value; callers wanting custom tuning should
// build the provider directly through internal/ranker (see
// recall.WithReranker for the public install path).
func NewLLMReranker(client llm.LLM) Reranker {
	return ranker.NewLLM(client)
}
