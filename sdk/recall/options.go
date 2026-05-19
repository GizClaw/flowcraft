package recall

import (
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/compiler"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// Option configures Memory at construction time.
//
// The defaults supplied by New build a fully in-memory v2 stack. The
// public options expose stable integration points only; lower-level
// store/compiler/source/projection injection remains package-internal
// until those contracts are ready to support external implementations.
type Option func(*config)

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
