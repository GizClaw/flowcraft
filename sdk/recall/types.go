package recall

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// ErrNotImplemented marks v2 surfaces whose architecture boundary exists
// before their full implementation lands.
var ErrNotImplemented = errors.New("recall: v2 implementation not yet wired")

// Scope identifies the tenant/user partition for canonical memory. It aliases
// the internal canonical model so the public facade does not duplicate schema.
type Scope = model.Scope

// FactKind classifies a canonical memory fact.
type FactKind = model.FactKind

const (
	FactEvent      FactKind = model.KindEvent
	FactState      FactKind = model.KindState
	FactPreference FactKind = model.KindPreference
	FactRelation   FactKind = model.KindRelation
	FactPlan       FactKind = model.KindPlan
	FactNote       FactKind = model.KindNote
)

// EvidenceRef points back to source material used to produce a fact.
type EvidenceRef = model.EvidenceRef

// TemporalFact is the public v2 memory unit. It is intentionally
// fact-shaped rather than Entry/retrieval-doc shaped. It aliases the internal
// canonical model; sdk/recall owns the public name, internal/model owns the
// schema definition.
type TemporalFact = model.TemporalFact

// SaveRequest is the v2 ingestion input. Higher-level chat integrations can
// build these from messages before calling Save.
type SaveRequest struct {
	Facts []TemporalFact
}

type SaveResult struct {
	FactIDs []string
}

type Query struct {
	Text     string
	Entities []string
	Limit    int
}

type Hit struct {
	Fact  TemporalFact
	Score float64
}

// Memory is the v2 fact-centric facade.
type Memory interface {
	Save(ctx context.Context, scope Scope, req SaveRequest) (SaveResult, error)
	Recall(ctx context.Context, scope Scope, query Query) ([]Hit, error)
	Forget(ctx context.Context, scope Scope, factID string) error
	Close() error
}

type Option func(*config)

type config struct{}

type memory struct{}

// New constructs a v2 Memory. The concrete implementation will be wired in
// subsequent PRs; this skeleton exists so package boundaries can compile.
func New(opts ...Option) (Memory, error) {
	var cfg config
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return &memory{}, nil
}

func (m *memory) Save(context.Context, Scope, SaveRequest) (SaveResult, error) {
	return SaveResult{}, ErrNotImplemented
}

func (m *memory) Recall(context.Context, Scope, Query) ([]Hit, error) {
	return nil, ErrNotImplemented
}

func (m *memory) Forget(context.Context, Scope, string) error {
	return ErrNotImplemented
}

func (m *memory) Close() error { return nil }
