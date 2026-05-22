// Package scoring is deprecated.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/retrieval/scoring instead.
// This compatibility package will be removed in v0.5.0.
package scoring

import target "github.com/GizClaw/flowcraft/memory/retrieval/scoring"

const (
	DefaultRRFK = target.DefaultRRFK
)

var (
	CosineSim      = target.CosineSim
	DotProduct     = target.DotProduct
	EuclideanDist  = target.EuclideanDist
	RRF            = target.RRF
	WeightedFusion = target.WeightedFusion
)
