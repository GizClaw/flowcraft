package port

import (
	"context"
	"time"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

// IngestInput drives a single Ingestor.Ingest call.
//
// Facts is the structured channel (passthrough extractor); Turns is
// the typed per-turn channel consumed by LLM-backed extractors.
// Empty channels are valid — the default extractor just returns
// caller-supplied Facts unchanged.
type IngestInput struct {
	Scope         domain.Scope
	Facts         []domain.TemporalFact
	Turns         []domain.TurnContext
	ObservedAt    time.Time
	KnownEntities []EntitySnapshot
	Now           time.Time

	// Tier is the SaveRequest importance intent label (Phase D.3).
	// Empty means "general". Mapped to Confidence in salience scoring.
	Tier string

	// RecentMessages is caller-supplied prior-turn context for the LLM
	// extractor (Phase D.7). Recall does not fetch history itself.
	RecentMessages []domain.Message

	// ExistingFactsAnchor is caller-supplied dedup anchors for extract.
	ExistingFactsAnchor []domain.TemporalFact
}

// IngestResult is what the ingest pipeline returns.
//
// Dropped uses the canonical diagnostic.DroppedFact (Fact any);
// subsystem read sites type-assert against domain.TemporalFact to
// recover the concrete shape. See diagnostic/shared.go for why
// the field is opaque (avoiding the diagnostic→domain import
// cycle).
type IngestResult struct {
	Facts                []domain.TemporalFact
	Dropped              []diagnostic.DroppedFact
	StructurizerCoverage diagnostic.StructurizerCoverage
}

// TurnContext re-exports the canonical domain.TurnContext for
// IngestInput.Turns consumers; the canonical type lives in
// internal/domain/turn_context.go so the public recall facade can
// alias it without referencing port.
type TurnContext = domain.TurnContext

// Ingestor owns the write-time ingest pipeline. Concrete
// implementations live in internal/ingest/.
type Ingestor interface {
	Compile(ctx context.Context, input IngestInput) (IngestResult, error)
}

// Extractor turns raw input into candidate facts.
type Extractor interface {
	Extract(ctx context.Context, input IngestInput) ([]domain.TemporalFact, error)
}

// Structurizer fills the structural fields the slim LLM extractor
// does not own (entities, subject/predicate/object, valid_from
// hints), and acts as a keyword-based fallback for Kind when the
// LLM left it empty.
type Structurizer interface {
	Structurize(f domain.TemporalFact, input IngestInput) domain.TemporalFact
}

// Normalizer canonicalises free-form fields so deterministic merge
// keys produce stable identities.
type Normalizer interface {
	Normalize(f domain.TemporalFact) domain.TemporalFact
}

// PredicateSynonyms maps any equivalent predicate spellings to a
// single canonical form.
type PredicateSynonyms interface {
	Canonical(predicate string) string
}

// EntityResolver maps surface mentions to scope-local canonical
// entity identifiers.
type EntityResolver interface {
	Resolve(f domain.TemporalFact) domain.TemporalFact
}

// AliasResolver canonicalises a single surface mention within a
// scope. Implementations stay pure (no I/O); they are consulted
// once per mention per Ingest call.
type AliasResolver interface {
	Canonical(scope domain.Scope, mention string) string
}

// EntityExtractor mines entity mentions from a fact's content
// during the Structurizer stage.
type EntityExtractor interface {
	ExtractEntities(content string, known []EntitySnapshot) []string
}

// TimeResolver normalises ObservedAt / ValidFrom / ValidTo on a
// fact, parsing any relative-time hints the upstream extractor /
// structurizer placed on the fact metadata.
type TimeResolver interface {
	Resolve(f domain.TemporalFact, now time.Time) domain.TemporalFact
}

// SalienceScorer assigns the confidence / promotion weight that
// downstream telemetry can rely on as a stable floor.
type SalienceScorer interface {
	Score(f domain.TemporalFact) domain.TemporalFact
}

// IDGenerator mints canonical fact identifiers. Tests inject
// deterministic generators.
type IDGenerator interface {
	NewID(f domain.TemporalFact, now time.Time) string
}

// ConflictResolver consults a read-only View to apply the canonical
// idempotency rules.
type ConflictResolver interface {
	ResolveConflicts(ctx context.Context, view View, facts []domain.TemporalFact) (domain.Resolution, error)
}

// View is the minimal read-only surface ConflictResolver requires
// from the canonical store. Keeping it narrow lets the resolver
// stay free of mutation side-effects.
type View interface {
	FindByMergeKey(ctx context.Context, scope domain.Scope, mergeKey string) ([]domain.TemporalFact, error)
	Get(ctx context.Context, scope domain.Scope, factID string) (domain.TemporalFact, error)
}
