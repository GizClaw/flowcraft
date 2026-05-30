package pipeline

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
	base "github.com/GizClaw/flowcraft/sdk/retrieval/pipeline"
)

type (
	// Reranker reorders a candidate set after retrieval and boosting.
	Reranker = base.Reranker
	// LLMReranker is the LLM-backed reranker used by recall LTM.
	LLMReranker = base.LLMReranker
)

// LTMOption mutates the recall long-term-memory pipeline recipe.
type LTMOption = any

type optionFunc func(*ltmConfig)

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
	entityLinkLane           bool
	entityLinkResolver       EntityLinkResolver
	entityLinkLaneTopK       int
	entityLinkPerEntityCap   int
	entityLinkBoost          float64
}

func WithRecallTopK(k int) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.recallTopK = k })
}
func WithBM25Weight(w float64) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.bm25Weight = w })
}
func WithEntityBoost(w float64) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.entityBoost = w })
}
func WithScoreThreshold(min float64) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.scoreThreshold = min })
}
func WithSupersededDecay(factor float64) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.supersededFactor = factor })
}
func WithTimeDecayHalfLife(hl time.Duration) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.halfLife = hl })
}
func WithReranker(r Reranker) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.reranker = r })
}
func WithEntityExtractor(extract func(ctx context.Context, text string) ([]string, error)) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.entityExtract = EntityExtract{LLMExtractor: extract} })
}
func WithLimit(k int) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.limit = k })
}
func WithMultiRecall(on bool) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.multiRecall = on })
}
func WithBM25LaneTopK(k int) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.bm25LaneTopK = k })
}
func WithEntityLaneTopK(k int) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.entityLaneTopK = k })
}
func WithRRFK(k float64) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.rrfK = k })
}
func WithEntityLaneMinSelectivity(ratio float64) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.entityLaneMinSelectivity = ratio })
}
func WithEntityLinkLane(on bool) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.entityLinkLane = on })
}
func WithEntityLinkResolver(r EntityLinkResolver) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.entityLinkResolver = r })
}
func WithEntityLinkLaneTopK(k int) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.entityLinkLaneTopK = k })
}
func WithEntityLinkPerEntityCap(n int) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.entityLinkPerEntityCap = n })
}
func WithEntityLinkBoost(weight float64) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.entityLinkBoost = weight })
}
func WithSlotCollapse(on bool) LTMOption {
	return optionFunc(func(c *ltmConfig) { c.slotCollapse = on })
}

// LTM returns recall's long-term-memory retrieval pipeline.
func LTM(emb embedding.Embedder, opts ...LTMOption) *base.Pipeline {
	cfg := ltmConfig{
		recallTopK:       60,
		bm25Weight:       0.3,
		entityBoost:      0.4,
		scoreThreshold:   0.05,
		supersededFactor: 0.3,
		halfLife:         30 * 24 * time.Hour,
		limit:            10,
	}
	var legacy []base.LTMOption
	for _, o := range opts {
		switch opt := o.(type) {
		case nil:
		case optionFunc:
			opt(&cfg)
		case func(*ltmConfig):
			opt(&cfg)
		case base.LTMOption:
			legacy = append(legacy, opt)
		}
	}
	if len(legacy) > 0 {
		return base.LTM(emb, legacy...)
	}
	primaryMode := base.ModeVector
	primaryLane := string(retrieval.LaneVector)
	if emb == nil {
		primaryMode = base.ModeBM25
		primaryLane = string(retrieval.LaneBM25)
	}
	useMultiRecall := cfg.multiRecall && emb != nil

	var stages []base.Stage
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
		entSelect := cfg.entityLaneMinSelectivity
		if entSelect == 0 {
			entSelect = 0.1
		} else if entSelect < 0 {
			entSelect = 0
		}
		lanes := base.MultiRetrieve{
			string(retrieval.LaneVector): {Mode: base.ModeVector, TopK: cfg.recallTopK},
			string(retrieval.LaneBM25):   {Mode: base.ModeBM25, TopK: bm25K},
			string(retrieval.LaneEntity): {Mode: base.ModeEntity, TopK: entK, MinSelectivity: entSelect},
		}
		stages = []base.Stage{
			base.HybridShortCircuit{},
			&base.EmbedQuery{Embedder: emb},
			cfg.entityExtract,
		}
		useEntityLinkBoost := false
		if cfg.entityLinkLane && cfg.entityLinkResolver != nil {
			perEntityCap := cfg.entityLinkPerEntityCap
			if perEntityCap <= 0 {
				perEntityCap = entityLinkLookupDefaultCap
			}
			stages = append(stages, EntityLinkLookup{
				Resolver:     cfg.entityLinkResolver,
				PerEntityCap: perEntityCap,
			})
			if cfg.entityLinkBoost > 0 {
				useEntityLinkBoost = true
			} else {
				linkK := cfg.entityLinkLaneTopK
				if linkK <= 0 {
					linkK = 30
				}
				lanes[string(retrieval.LaneEntityLink)] = base.RetrieveSpec{
					Mode: base.ModeEntityLink,
					TopK: linkK,
				}
			}
		}
		stages = append(stages, lanes, base.RRFFusion{K: rrfK})
		if useEntityLinkBoost {
			stages = append(stages, EntityLinkBoost{Boost: cfg.entityLinkBoost})
		}
	} else {
		stages = []base.Stage{
			base.HybridShortCircuit{},
			&base.EmbedQuery{Embedder: emb},
			cfg.entityExtract,
			base.Retrieve{Lane: primaryLane, Spec: base.RetrieveSpec{Mode: primaryMode, TopK: cfg.recallTopK}},
			liftRecall{Lane: primaryLane},
		}
		addBM25Boost = primaryMode != base.ModeBM25 && cfg.bm25Weight > 0
	}
	if addBM25Boost {
		stages = append(stages, base.BM25Boost{Weight: cfg.bm25Weight})
	}
	if cfg.entityBoost > 0 {
		stages = append(stages, EntityBoost{Boost: cfg.entityBoost})
	}
	if cfg.scoreThreshold > 0 {
		stages = append(stages, base.ScoreThreshold{Min: cfg.scoreThreshold})
	}
	if cfg.supersededFactor > 0 && cfg.supersededFactor < 1 {
		stages = append(stages, SupersededDecay{Factor: cfg.supersededFactor})
	}
	if cfg.slotCollapse {
		stages = append(stages, SlotCollapse{})
	}
	if cfg.halfLife > 0 {
		stages = append(stages, base.TimeDecay{HalfLife: cfg.halfLife})
	}
	if cfg.reranker != nil {
		stages = append(stages, base.Rerank{Reranker: cfg.reranker})
	}
	stages = append(stages, base.Limit{TopK: cfg.limit})
	return base.New(stages...)
}

type liftRecall struct {
	Lane string
}

func (s liftRecall) Name() string { return "LiftRecall" }

func (s liftRecall) Run(_ context.Context, st *base.State) error {
	hits := st.Recalls[s.Lane]
	if len(hits) == 0 {
		return nil
	}
	st.Final = cloneHits(hits)
	return nil
}
