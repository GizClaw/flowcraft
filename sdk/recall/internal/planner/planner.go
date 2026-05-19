// Package planner turns a caller Query into a deterministic
// QueryPlan. PR-3 ships a rule-based planner; the boundary keeps
// shape stable for an opt-in LLM intent parser in later phases
// (docs §9.1).
package planner

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Source identifiers known to PR-3. New source names should be
// declared alongside their source implementation so the planner has
// a single registry to draw from.
const (
	SourceRetrieval = "retrieval"
	SourceEntity    = "entity"
)

// DefaultLimit applies when a caller leaves Query.Limit == 0. PR-3
// keeps it conservative so naïve callers do not accidentally fan
// out a huge candidate set; explicit values up to MaxLimit are
// honoured verbatim.
const DefaultLimit = 10

// MaxLimit is the hard cap on returned hits. Callers passing a
// larger Query.Limit are silently clamped, matching the docs §9
// guidance to keep recall responsive.
const MaxLimit = 100

// Input is the planner contract. It is kept separate from the public
// recall.Query so the planner can grow internal hints without
// touching the public API.
type Input struct {
	Scope    model.Scope
	Text     string
	Entities []string
	Limit    int
}

// Planner produces a QueryPlan. PR-3 ships RuleBased; alternative
// planners (LLM-driven, profile-specific) plug behind this
// interface.
type Planner interface {
	Plan(ctx context.Context, input Input) (model.QueryPlan, error)
}

// RuleBased is the deterministic Phase 3 planner.
//
// Source order:
//   - retrieval is always on
//   - entity runs only when Input.Entities is non-empty
//
// PR-3 deliberately does not extract entities from Text — that
// would silently introduce an LLM dependency in the read path. The
// caller is responsible for passing structured entity hints.
type RuleBased struct {
	// RetrievalShare controls the candidate budget allocated to the
	// retrieval source when entity is also active. Defaults to 0.6
	// so retrieval still gets the larger slice while entity backs
	// it up.
	RetrievalShare float64
}

// New returns the default rule-based planner.
func New() *RuleBased { return &RuleBased{RetrievalShare: 0.6} }

// Plan returns the QueryPlan with normalized limits and per-source
// budgets. Returns a deterministic, non-nil map even when only one
// source is active so downstream callers can avoid nil checks.
func (r *RuleBased) Plan(_ context.Context, input Input) (model.QueryPlan, error) {
	limit := input.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	share := r.RetrievalShare
	if share <= 0 || share >= 1 {
		share = 0.6
	}

	order := []string{SourceRetrieval}
	budgets := map[string]int{SourceRetrieval: limit}

	if len(input.Entities) > 0 {
		order = append(order, SourceEntity)
		// Clean per-source budgets that respect RetrievalShare
		// exactly: retrieval gets round(limit*share), entity gets
		// the remainder. Both clamp to at least 1 so neither source
		// is silently disabled on small limits — fusion's TotalCap
		// still enforces the user's limit if 1+1 > limit.
		retrievalBudget := maxInt(1, int(float64(limit)*share+0.5))
		entityBudget := maxInt(1, limit-retrievalBudget)
		budgets[SourceRetrieval] = retrievalBudget
		budgets[SourceEntity] = entityBudget
	}

	return model.QueryPlan{
		Intent: model.QueryIntent{
			Text:     input.Text,
			Entities: input.Entities,
			Scope:    input.Scope,
			Limit:    limit,
		},
		SourceOrder:   order,
		SourceBudgets: budgets,
		TotalCap:      limit,
	}, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
