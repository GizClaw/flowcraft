package recall

import (
	"github.com/GizClaw/flowcraft/memory/recall/internal/governance"
	"github.com/GizClaw/flowcraft/memory/recall/internal/ingest"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/ranker"
	"github.com/GizClaw/flowcraft/memory/recall/internal/store/asyncsemantic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/store/sideeffect"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/text/timex"
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
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

// llmExtractorConfig captures public LLM extractor options so New can defer
// concrete extractor selection until the default compiler is wired.
type llmExtractorConfig struct {
	client       llm.LLM
	mode         LLMExtractionMode
	systemPrompt string
	schemaName   string
	temperature  float64
	extraOptions []llm.GenerateOption
}

func (c *llmExtractorConfig) build() port.Extractor {
	if c == nil || c.client == nil {
		return nil
	}
	if c.mode == LLMExtractionTwoPass {
		ex := ingest.NewTwoPassLLMExtractor(c.client)
		if c.systemPrompt != "" {
			ex.FactSystem = c.systemPrompt
		}
		if c.schemaName != "" {
			ex.FactSchemaName = c.schemaName
			ex.AssertionSchemaName = c.schemaName + "_assertions"
			ex.KindSchemaName = c.schemaName + "_kinds"
			ex.RelationSchemaName = c.schemaName + "_relations"
			ex.EntitySchemaName = c.schemaName + "_entities"
			ex.EvidenceSchemaName = c.schemaName + "_evidence"
		}
		ex.Temperature = c.temperature
		ex.ExtraOptions = append(ex.ExtraOptions, c.extraOptions...)
		return ex
	}
	ex := ingest.NewLLMExtractor(c.client)
	if c.systemPrompt != "" {
		ex.System = c.systemPrompt
	}
	if c.schemaName != "" {
		ex.SchemaName = c.schemaName
	}
	ex.Temperature = c.temperature
	ex.ExtraOptions = append(ex.ExtraOptions, c.extraOptions...)
	return ex
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

func withExtraProjection(p port.Projection) Option {
	return func(c *config) {
		if p != nil {
			c.extraProjections = append(c.extraProjections, p)
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

// WithEvidenceStore installs an optional secondary evidence lookup
// adapter. Save keeps embedded EvidenceRefs authoritative; adapter
// write failures are telemetry-only and RebuildAll can rehydrate the
// adapter from canonical facts.
//
// Memory.Close calls Close on the installed evidence store.
func WithEvidenceStore(s EvidenceStore) Option {
	return func(c *config) {
		c.evidenceStore = s
	}
}

// WithRetrievalIndex overrides the retrieval backend that powers the
// retrieval projection. The default is memory/retrieval/memory.New().
//
// Memory.Close calls Close on the installed index. Share an index across
// Memory instances only when the caller owns the surrounding lifecycle.
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
	apply func(*llmExtractorConfig)
}

func newLLMExtractorOption(apply func(*llmExtractorConfig)) LLMExtractorOption {
	return LLMExtractorOption{apply: apply}
}

// LLMExtractionMode selects the LLM extraction strategy used by
// WithLLMExtractor.
type LLMExtractionMode string

const (
	// LLMExtractionSinglePass uses the existing single prompt that emits
	// text, kind, and evidence refs in one model call. This is the default.
	LLMExtractionSinglePass LLMExtractionMode = "single_pass"
	// LLMExtractionTwoPass first extracts text+kind, then runs a shorter
	// grounding prompt to attach evidence turn ids.
	LLMExtractionTwoPass LLMExtractionMode = "two_pass"
)

// WithLLMExtractionMode selects the extraction strategy. Unknown values
// fall back to the single-pass extractor.
func WithLLMExtractionMode(mode LLMExtractionMode) LLMExtractorOption {
	return newLLMExtractorOption(func(c *llmExtractorConfig) {
		c.mode = mode
	})
}

// WithLLMExtractorSystemPrompt overrides the default fact extraction
// system prompt. In two-pass mode, evidence grounding keeps its shorter
// SDK-managed prompt.
func WithLLMExtractorSystemPrompt(prompt string) LLMExtractorOption {
	return newLLMExtractorOption(func(c *llmExtractorConfig) {
		if prompt != "" {
			c.systemPrompt = prompt
		}
	})
}

// WithLLMExtractorTemperature sets the sampling temperature. Zero
// means "use provider default".
func WithLLMExtractorTemperature(t float64) LLMExtractorOption {
	return newLLMExtractorOption(func(c *llmExtractorConfig) { c.temperature = t })
}

// WithLLMExtractorSchemaName labels the JSON schema for structured
// output (some providers display this in their dashboards / logs). In
// two-pass mode, the evidence schema name is derived with "_evidence".
func WithLLMExtractorSchemaName(name string) LLMExtractorOption {
	return newLLMExtractorOption(func(c *llmExtractorConfig) {
		if name != "" {
			c.schemaName = name
		}
	})
}

// WithLLMExtractorExtraOptions forwards provider-specific
// llm.GenerateOption values on every extraction call (e.g. provider
// extra params, reasoning toggles).
func WithLLMExtractorExtraOptions(opts ...llm.GenerateOption) LLMExtractorOption {
	return newLLMExtractorOption(func(c *llmExtractorConfig) {
		c.extraOptions = append(c.extraOptions, opts...)
	})
}

// WithLLMExtractor enables LLM-driven fact extraction in the
// default compiler pipeline. The supplied client is consulted on
// Save calls whose SaveRequest carries non-empty Turns; the
// extractor renders Turns into a canonical JSONL wire shape and
// asks the model to emit minimal memories and supporting evidence ids.
//
// nil client falls back to the deterministic passthrough extractor.
func WithLLMExtractor(client llm.LLM, opts ...LLMExtractorOption) Option {
	return func(c *config) {
		if client == nil {
			return
		}
		cfg := &llmExtractorConfig{client: client}
		for _, opt := range opts {
			if opt.apply != nil {
				opt.apply(cfg)
			}
		}
		c.llmExtractor = cfg
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
//
// Memory.Close does not drain or close this queue; the caller-owned worker or
// adapter owns queue shutdown.
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
//
// Memory.Close does not drain or close this outbox; callers should stop
// ProcessSideEffects workers and drain according to the adapter's own contract.
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
