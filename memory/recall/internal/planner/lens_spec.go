package planner

import "github.com/GizClaw/flowcraft/memory/recall/internal/domain"

// LensSpec describes one recall lens for the rule-based planner:
// activation predicate, fusion weight, and stable source name.
// Registration order in lens.Registry defines planner source order
// (retrieval first, then entity, graph, relation, profile, timeline).
type LensSpec struct {
	Name     string
	Weight   float64
	Activate func(intent domain.QueryIntent) bool
}
