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
//
// When [WithMultiRecall] is enabled, the recall topology switches to
//
//	MultiRetrieve(vector top 60, bm25 top 50, entity top 30) → RRFFusion(K=60) →
//	entity boost → ScoreThreshold → SupersededDecay → TimeDecay → (optional) Rerank →
//	Limit{TopK: 10}
//
// (BM25 boost is suppressed under multi-recall because BM25 is now a recall
// lane and a post-fusion BM25 boost would double-count its contribution.)
type LTMOption func(*ltmConfig)

type ltmConfig struct {
	recallTopK               int
	bm25Weight               float64
	entityBoost              float64
	scoreThreshold           float64
	supersededFactor         float64
	halfLife                 time.Duration
	limit                    int
	reranker                 Reranker
	entityExtract            EntityExtract
	slotCollapse             bool
	multiRecall              bool
	bm25LaneTopK             int
	entityLaneTopK           int
	rrfK                     float64
	entityLaneMinSelectivity float64
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

// WithMultiRecall switches the LTM recall topology from "single-lane
// vector recall + BM25/entity boosts" to "three-lane recall
// (vector + bm25 + entity) followed by Reciprocal-Rank-Fusion".
// Defaults to false (preserves the legacy single-lane behavior).
//
// The infrastructure for all three lanes already existed in this
// package (MultiRetrieve, ModeEntity, RRFFusion); this option just
// wires them into the LTM recipe so callers can opt into a hybrid
// recall topology without hand-assembling a custom Pipeline.
//
// When enabled:
//   - BM25 is moved from boost-time to recall-time. The BM25 lane
//     fan-out is controlled by [WithBM25LaneTopK] (default 50).
//   - The entity lane fan-out is controlled by [WithEntityLaneTopK]
//     (default 30). The entity lane filters memories by entity-set
//     overlap (Doc.Metadata["entities"] ContainsAny QueryEntities),
//     so it depends on docs having an `entities` field populated at
//     ingest time (sdk/recall does this automatically when its
//     extractor returns the entities field).
//   - Vector recall fan-out is taken from [WithRecallTopK] (default
//     60) so the primary semantic channel keeps the same budget as
//     legacy LTM.
//   - The post-recall BM25Boost is suppressed (BM25 already
//     contributed to the fused score; boosting again double-counts).
//     EntityBoost is kept — it operates on the fused score and
//     provides a final entity-overlap nudge.
//   - RRFFusion's K is taken from [WithRRFK] (default 60).
//
// Falls back to single-lane recall when the embedder is nil (no
// vector lane means the multi-recall topology degrades to "bm25 +
// entity", at which point the legacy BM25-only path is simpler).
func WithMultiRecall(on bool) LTMOption {
	return func(c *ltmConfig) { c.multiRecall = on }
}

// WithBM25LaneTopK overrides the BM25 recall-lane fan-out under
// [WithMultiRecall] (default 50). No-op when multi-recall is off.
func WithBM25LaneTopK(k int) LTMOption {
	return func(c *ltmConfig) { c.bm25LaneTopK = k }
}

// WithEntityLaneTopK overrides the entity recall-lane fan-out under
// [WithMultiRecall] (default 30). No-op when multi-recall is off.
func WithEntityLaneTopK(k int) LTMOption {
	return func(c *ltmConfig) { c.entityLaneTopK = k }
}

// WithRRFK overrides the K parameter of RRFFusion under
// [WithMultiRecall] (default 60). Lower K weights top-ranked hits
// more aggressively; the default 60 matches the published RRF paper.
// No-op when multi-recall is off.
func WithRRFK(k float64) LTMOption {
	return func(c *ltmConfig) { c.rrfK = k }
}

// WithEntityLaneMinSelectivity gates the entity recall lane on
// query-side IDF selectivity. The entity lane fires only when at
// least one query atom is "rare" within the namespace — i.e. appears
// in strictly fewer than `ratio * N` docs (N = universe size under
// the request filter).
//
// Defaults to 0.1 under [WithMultiRecall] (atom must match < 10% of
// namespace). Pass 0 to disable the gate (legacy behaviour: lane
// fires for any non-empty QueryEntities).
//
// Background: the LoCoMo 25866478422 ablation showed that even
// IDF-weighted entity recall regressed qa.judge by 17 pp because
// queries dominated by universal atoms (`tuesday`, `morning`,
// `favorite`, `food`) flooded the lane with low-information
// candidates whose RRF rank vote displaced vector's precision
// picks. Gating on selectivity collapses those queries back to
// "lane returns nothing", leaving the fused result driven by
// vector + BM25 alone — see
// internal-docs/eval-entity-lane-ablation-2026-05-14.md.
func WithEntityLaneMinSelectivity(ratio float64) LTMOption {
	return func(c *ltmConfig) { c.entityLaneMinSelectivity = ratio }
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
	// working. The multi-recall topology also requires an embedder
	// (otherwise it degenerates to "bm25 + entity", which the legacy
	// single-lane BM25 path covers more simply) — fall back to legacy in
	// that case rather than emit a partially-realised multi-recall.
	primaryMode := ModeVector
	primaryLane := string(retrieval.LaneVector)
	if emb == nil {
		primaryMode = ModeBM25
		primaryLane = string(retrieval.LaneBM25)
	}
	useMultiRecall := cfg.multiRecall && emb != nil

	var stages []Stage
	addBM25Boost := false
	if useMultiRecall {
		bm25K := cfg.bm25LaneTopK
		if bm25K <= 0 {
			bm25K = 50
		}
		entK := cfg.entityLaneTopK
		if entK <= 0 {
			entK = 30
		}
		rrfK := cfg.rrfK
		if rrfK <= 0 {
			rrfK = 60
		}
		// Default entity-lane selectivity gate: 10% of namespace.
		// Pass `WithEntityLaneMinSelectivity(0)` to disable.
		entSelect := cfg.entityLaneMinSelectivity
		if entSelect == 0 {
			entSelect = 0.1
		} else if entSelect < 0 {
			entSelect = 0
		}
		stages = []Stage{
			HybridShortCircuit{},
			&EmbedQuery{Embedder: emb},
			cfg.entityExtract,
			MultiRetrieve{
				string(retrieval.LaneVector): {Mode: ModeVector, TopK: cfg.recallTopK},
				string(retrieval.LaneBM25):   {Mode: ModeBM25, TopK: bm25K},
				string(retrieval.LaneEntity): {Mode: ModeEntity, TopK: entK, MinSelectivity: entSelect},
			},
			RRFFusion{K: rrfK},
		}
		// Under multi-recall, BM25 is a recall lane, not a boost.
		// Boosting again would double-count its contribution.
		// EntityBoost is intentionally kept downstream because it
		// re-scales the fused score by overlap count rather than
		// inserting a duplicate signal.
	} else {
		stages = []Stage{
			HybridShortCircuit{},
			&EmbedQuery{Embedder: emb},
			cfg.entityExtract,
			Retrieve{Lane: primaryLane, Spec: RetrieveSpec{Mode: primaryMode, TopK: cfg.recallTopK}},
			// Lift the recall lane into Final so subsequent boosts operate
			// on a single ranked list (vector-first; BM25/entity are boost
			// signals, not recall expanders).
			liftRecall{Lane: primaryLane},
		}
		// BM25Boost is a re-ranking signal layered on top of the recall lane;
		// when the recall lane is itself BM25 the boost would double-count, so
		// we suppress it. This expresses "BM25 is a complement to vector
		// recall, not its own additive lane" in one place.
		addBM25Boost = primaryMode != ModeBM25 && cfg.bm25Weight > 0
	}
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
