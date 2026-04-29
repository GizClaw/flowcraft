package pipeline

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// LTMOption mutates the LTM pipeline configuration before assembly.
//
// Options compose; later options win. The defaults assemble:
//
//	vector-only recall (top 60) → BM25 boost → entity boost →
//	ScoreThreshold 0.05 → SupersededDecay → TimeDecay 30d → (optional) Rerank →
//	Limit{TopK: 10}
type LTMOption func(*ltmConfig)

type ltmConfig struct {
	recallTopK       int
	bm25Weight       float64
	entityBoost      float64
	scoreThreshold   float64
	supersededFactor float64
	halfLife         time.Duration
	limit            int
	reranker         Reranker
	entityExtract    EntityExtract
	slotCollapse     bool
}

// WithRecallTopK overrides the vector recall fan-out (default 60).
func WithRecallTopK(k int) LTMOption {
	return func(c *ltmConfig) { c.recallTopK = k }
}

// WithBM25Weight overrides the BM25 boost weight (default 0.3, 0 disables).
func WithBM25Weight(w float64) LTMOption {
	return func(c *ltmConfig) { c.bm25Weight = w }
}

// WithEntityBoost overrides the entity boost weight (default 0.4, 0 disables).
func WithEntityBoost(w float64) LTMOption {
	return func(c *ltmConfig) { c.entityBoost = w }
}

// WithScoreThreshold drops candidates below this score before rerank/limit
// (default 0.05).
func WithScoreThreshold(min float64) LTMOption {
	return func(c *ltmConfig) { c.scoreThreshold = min }
}

// WithSupersededDecay sets the score multiplier for memories whose
// metadata.superseded_by is non-empty (default 0.3).
func WithSupersededDecay(factor float64) LTMOption {
	return func(c *ltmConfig) { c.supersededFactor = factor }
}

// WithTimeDecayHalfLife overrides the time-decay half-life (default 30 days).
// Pass 0 to disable.
func WithTimeDecayHalfLife(hl time.Duration) LTMOption {
	return func(c *ltmConfig) { c.halfLife = hl }
}

// WithReranker installs an LLM/cross-encoder reranker run after boosts.
func WithReranker(r Reranker) LTMOption {
	return func(c *ltmConfig) { c.reranker = r }
}

// WithEntityExtractor installs a custom query-side entity extractor.
//
// Default: rule-based extraction. Pass an LLM-driven extractor to improve
// recall on noisy / multilingual queries.
func WithEntityExtractor(extract func(ctx context.Context, text string) ([]string, error)) LTMOption {
	return func(c *ltmConfig) {
		c.entityExtract = EntityExtract{LLMExtractor: extract}
	}
}

// WithLimit overrides the final TopK truncation (default 10).
func WithLimit(k int) LTMOption {
	return func(c *ltmConfig) { c.limit = k }
}

// WithSlotCollapse inserts a [SlotCollapse] stage after
// [SupersededDecay] so legacy entries that were never tagged with
// superseded_by still get collapsed to the newest hit per
// (subject, predicate) tuple at recall time. Defaults to false; enable
// when running on data written before the slot supersede channel
// shipped or when the underlying writer cannot guarantee tagging.
func WithSlotCollapse(on bool) LTMOption {
	return func(c *ltmConfig) { c.slotCollapse = on }
}

// Default returns the general-purpose hybrid pipeline.
//
//	EmbedQuery → EntityExtract → MultiRetrieve(bm25,vector,entity) →
//	RRFFusion → EntityBoost → Limit
func Default(emb embedding.Embedder) *Pipeline {
	return New(
		HybridShortCircuit{},
		&EmbedQuery{Embedder: emb},
		EntityExtract{},
		MultiRetrieve{
			string(retrieval.LaneBM25):   {Mode: ModeBM25, TopK: 50},
			string(retrieval.LaneVector): {Mode: ModeVector, TopK: 50},
			string(retrieval.LaneEntity): {Mode: ModeEntity, TopK: 30},
		},
		RRFFusion{K: 60},
		EntityBoost{Boost: 0.2},
		Limit{TopK: 10},
	)
}

// LTM returns the long-term-memory pipeline. The default recipe is
// vector-first recall with BM25 + entity acting as ranking boosts (not
// recall expanders), then SupersededDecay (soft-merge) and TimeDecay,
// optionally followed by an LLM reranker, ending in Limit.
//
// Old positional signature `LTM(emb)` continues to work — variadic options
// default to the recipe described above.
func LTM(emb embedding.Embedder, opts ...LTMOption) *Pipeline {
	cfg := ltmConfig{
		recallTopK:       60,
		bm25Weight:       0.3,
		entityBoost:      0.4,
		scoreThreshold:   0.05,
		supersededFactor: 0.3,
		halfLife:         30 * 24 * time.Hour,
		limit:            10,
	}
	for _, o := range opts {
		o(&cfg)
	}

	// Without an embedder the vector lane is silent; fall back to BM25 recall
	// so memory tests / in-process indexes that ship without embeddings keep
	// working.
	primaryMode := ModeVector
	primaryLane := string(retrieval.LaneVector)
	if emb == nil {
		primaryMode = ModeBM25
		primaryLane = string(retrieval.LaneBM25)
	}

	stages := []Stage{
		HybridShortCircuit{},
		&EmbedQuery{Embedder: emb},
		cfg.entityExtract,
		Retrieve{Lane: primaryLane, Spec: RetrieveSpec{Mode: primaryMode, TopK: cfg.recallTopK}},
		// Lift the recall lane into Final so subsequent boosts operate on a
		// single ranked list (vector-first; BM25/entity are boost signals,
		// not recall expanders).
		liftRecall{Lane: primaryLane},
	}
	// BM25Boost is a re-ranking signal layered on top of the recall lane;
	// when the recall lane is itself BM25 the boost would double-count, so
	// we suppress it. This expresses "BM25 is a complement to vector
	// recall, not its own additive lane" in one place.
	addBM25Boost := primaryMode != ModeBM25 && cfg.bm25Weight > 0
	if addBM25Boost {
		stages = append(stages, BM25Boost{Weight: cfg.bm25Weight})
	}
	if cfg.entityBoost > 0 {
		stages = append(stages, EntityBoost{Boost: cfg.entityBoost})
	}
	if cfg.scoreThreshold > 0 {
		stages = append(stages, ScoreThreshold{Min: cfg.scoreThreshold})
	}
	if cfg.supersededFactor > 0 && cfg.supersededFactor < 1 {
		stages = append(stages, SupersededDecay{Factor: cfg.supersededFactor})
	}
	if cfg.slotCollapse {
		stages = append(stages, SlotCollapse{})
	}
	if cfg.halfLife > 0 {
		stages = append(stages, TimeDecay{HalfLife: cfg.halfLife})
	}
	if cfg.reranker != nil {
		stages = append(stages, Rerank{Reranker: cfg.reranker})
	}
	stages = append(stages, Limit{TopK: cfg.limit})
	return New(stages...)
}

// Knowledge returns the knowledge-base pipeline. When ce is
// non-nil, a CrossEncoder Rerank stage is appended before Limit.
func Knowledge(emb embedding.Embedder, ce Reranker) *Pipeline {
	stages := []Stage{
		HybridShortCircuit{},
		&EmbedQuery{Embedder: emb},
		EntityExtract{},
		MultiRetrieve{
			string(retrieval.LaneBM25):   {Mode: ModeBM25, TopK: 50},
			string(retrieval.LaneVector): {Mode: ModeVector, TopK: 50},
		},
		RRFFusion{K: 60},
		EntityBoost{Boost: 0.2},
	}
	if ce != nil {
		stages = append(stages, Rerank{Reranker: ce})
	}
	stages = append(stages, Limit{TopK: 10})
	return New(stages...)
}
