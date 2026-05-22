// Package pipeline is deprecated.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/retrieval/pipeline instead.
// This compatibility package will be removed in v0.5.0.
package pipeline

import target "github.com/GizClaw/flowcraft/memory/retrieval/pipeline"

type (
	BM25Boost          = target.BM25Boost
	ConvexFusion       = target.ConvexFusion
	Dedup              = target.Dedup
	EmbedQuery         = target.EmbedQuery
	EntityBoost        = target.EntityBoost
	EntityExtract      = target.EntityExtract
	EntityLinkBoost    = target.EntityLinkBoost
	EntityLinkLookup   = target.EntityLinkLookup
	EntityLinkResolver = target.EntityLinkResolver
	HybridShortCircuit = target.HybridShortCircuit
	LLMReranker        = target.LLMReranker
	LTMOption          = target.LTMOption
	Limit              = target.Limit
	MMR                = target.MMR
	MultiRetrieve      = target.MultiRetrieve
	Pipeline           = target.Pipeline
	PostFilter         = target.PostFilter
	QueryRewrite       = target.QueryRewrite
	RRFFusion          = target.RRFFusion
	Rerank             = target.Rerank
	Reranker           = target.Reranker
	Retrieve           = target.Retrieve
	RetrieveMode       = target.RetrieveMode
	RetrieveSpec       = target.RetrieveSpec
	ScoreThreshold     = target.ScoreThreshold
	SlotCollapse       = target.SlotCollapse
	Stage              = target.Stage
	StageTrace         = target.StageTrace
	State              = target.State
	SupersededDecay    = target.SupersededDecay
	TimeDecay          = target.TimeDecay
	WeightedFusion     = target.WeightedFusion
)

const (
	ModeBM25       = target.ModeBM25
	ModeEntity     = target.ModeEntity
	ModeEntityLink = target.ModeEntityLink
	ModeSparse     = target.ModeSparse
	ModeVector     = target.ModeVector
)

var (
	Default                      = target.Default
	Knowledge                    = target.Knowledge
	LTM                          = target.LTM
	New                          = target.New
	WithBM25LaneTopK             = target.WithBM25LaneTopK
	WithBM25Weight               = target.WithBM25Weight
	WithEntityBoost              = target.WithEntityBoost
	WithEntityExtractor          = target.WithEntityExtractor
	WithEntityLaneMinSelectivity = target.WithEntityLaneMinSelectivity
	WithEntityLaneTopK           = target.WithEntityLaneTopK
	WithEntityLinkBoost          = target.WithEntityLinkBoost
	WithEntityLinkLane           = target.WithEntityLinkLane
	WithEntityLinkLaneTopK       = target.WithEntityLinkLaneTopK
	WithEntityLinkPerEntityCap   = target.WithEntityLinkPerEntityCap
	WithEntityLinkResolver       = target.WithEntityLinkResolver
	WithLimit                    = target.WithLimit
	WithMultiRecall              = target.WithMultiRecall
	WithRRFK                     = target.WithRRFK
	WithRecallTopK               = target.WithRecallTopK
	WithReranker                 = target.WithReranker
	WithScoreThreshold           = target.WithScoreThreshold
	WithSlotCollapse             = target.WithSlotCollapse
	WithSupersededDecay          = target.WithSupersededDecay
	WithTimeDecayHalfLife        = target.WithTimeDecayHalfLife
)
