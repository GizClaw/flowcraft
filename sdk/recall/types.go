package recall

import (
	"errors"
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// ErrNotImplemented marks v2 surfaces whose architecture boundary
// exists before their full implementation lands.
var ErrNotImplemented = errors.New("recall: v2 implementation not yet wired")

// Scope identifies the tenant/user partition for canonical memory. It
// aliases the internal canonical model so the public facade does not
// duplicate schema.
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

// MergeHints are LLM-supplied hints about merge behaviour. They are
// schema-level metadata only and do not participate in canonical
// merge decisions.
type MergeHints = model.MergeHints

// TemporalFact is the public v2 memory unit. It aliases the internal
// canonical model — sdk/recall owns the public name, internal/model
// owns the schema definition.
type TemporalFact = model.TemporalFact

// SaveRequest is the v2 ingestion input. Higher-level integrations
// build these from raw messages before calling Save.
type SaveRequest struct {
	// Facts are caller-supplied structured facts. The default
	// passthrough extractor treats them as authoritative content
	// and runs them through the compiler for deterministic field
	// hardening (id, observed_at, merge_key, salience, policy).
	Facts []TemporalFact

	// Text is the optional free-form input consumed by opt-in
	// extractors (notably LLMExtractor wired via WithLLMExtractor).
	// The default passthrough extractor ignores Text — only
	// extractors that opt in to text-driven extraction read it,
	// so PR-2/PR-3 callers passing structured Facts only stay
	// unaffected.
	Text string
}

// SaveResult reports the canonical fact ids that were appended to the
// ledger. Dedupe/policy drops are not listed here; telemetry surfaces
// them via the projection hook.
type SaveResult struct {
	FactIDs []string
}

// TimeRange bounds timeline recall. Aliases model.TimeRange.
type TimeRange = model.TimeRange

// Query is the v2 recall input shape. Structured hints activate
// optional sources (timeline / relation / profile) via the planner;
// omitting them preserves PR-3 retrieval+entity behaviour.
type Query struct {
	Text      string
	Entities  []string
	Limit     int
	Subject   string
	Predicate string
	Object    string
	Kinds     []FactKind
	TimeRange TimeRange
}

// TimeRangeFrom is a convenience for building a half-open range.
func TimeRangeFrom(from, to time.Time) TimeRange {
	return TimeRange{From: from, To: to}
}

// Hit is a materialized recall result. Score semantics are owned by
// the fusion layer in PR-3; for PR-2 Recall returns ErrNotImplemented.
type Hit struct {
	Fact  TemporalFact
	Score float64
}
