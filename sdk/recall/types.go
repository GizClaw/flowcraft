package recall

import (
	"time"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// Scope identifies the tenant/user partition for canonical memory. It
// aliases the internal canonical model so the public facade does not
// duplicate schema.
type Scope = domain.Scope

// FactKind classifies a canonical memory fact.
type FactKind = domain.FactKind

const (
	FactEvent      FactKind = domain.KindEvent
	FactState      FactKind = domain.KindState
	FactPreference FactKind = domain.KindPreference
	FactRelation   FactKind = domain.KindRelation
	FactPlan       FactKind = domain.KindPlan
	FactNote       FactKind = domain.KindNote
)

// EvidenceRef points back to source material used to produce a fact.
type EvidenceRef = domain.EvidenceRef

// MergeHints are LLM-supplied hints about merge behaviour. They are
// schema-level metadata only and do not participate in canonical
// merge decisions.
type MergeHints = domain.MergeHints

// TemporalFact is the public v2 memory unit. It aliases the internal
// canonical model — sdk/recall owns the public name, internal/model
// owns the schema definition.
type TemporalFact = domain.TemporalFact

// TrustContext carries read-time visibility constraints (Phase D.2).
type TrustContext = domain.TrustContext

// TurnContext is the typed per-turn channel adapters use to feed
// the LLMExtractor. Each TurnContext carries an id, an optional
// absolute timestamp, the canonical speaker name, the conversational
// role, and the body text — the same information adapters used to
// bake into a prose "[<date>] <Speaker>:" prefix.
//
// Passing typed turns instead of prose lets the SDK render the LLM
// user message in a canonical JSONL shape (one source of truth) and
// lets the Structurizer use the typed Time/Speaker fields directly
// for valid_from resolution and Subject inference — the LLM stops
// doing regex archaeology on prose.
type TurnContext = port.TurnContext

// EntitySnapshot is a hint about an entity the canonical projection
// has already seen in this scope. The compiler uses snapshots to
// deduplicate freshly-extracted entities against historical
// canonical forms and to seed the Structurizer's NER pass with
// high-confidence matches. Snapshots are a soft hint — missing /
// outdated entries only mean less canonicalization, not extraction
// failure.
type EntitySnapshot = port.EntitySnapshot

// SaveRequest is the v2 ingestion input. Higher-level integrations
// build these from raw messages before calling Save.
//
// Two input channels, by purpose (Memory.Save runs both):
//
//  1. Facts — fully-structured TemporalFacts (passthrough path).
//     The default extractor returns them verbatim; the compiler
//     still hardens id / merge_key / time / policy. Callers who
//     already produce structured facts (rule-based pipelines,
//     migration tooling, tests) use this channel exclusively.
//  2. Turns — typed per-turn metadata (id, time, speaker, role,
//     text). The LLMExtractor renders these into a canonical JSONL
//     wire shape for the model and feeds the typed Time / Speaker
//     fields into the Structurizer so the LLM never has to grep
//     timestamps or speakers out of prose. Adapters with raw chat
//     dumps just pass a single TurnContext per message; adapters
//     without per-turn metadata pass a single TurnContext with
//     only Text populated.
//
// There is intentionally no separate Text channel: a free-form
// paragraph is just a single TurnContext with Text set. Carrying
// both Text and Turns would be two paths for the same thing and
// leave callers wondering which one wins.
type SaveRequest struct {
	// Facts are caller-supplied structured facts. The default
	// passthrough extractor treats them as authoritative content
	// and runs them through the compiler for deterministic field
	// hardening (id, observed_at, merge_key, salience, policy).
	Facts []TemporalFact

	// Turns is the typed per-turn channel consumed by opt-in
	// extractors (notably LLMExtractor wired via WithLLMExtractor).
	// The default passthrough extractor ignores Turns — only
	// extractors that opt in to text-driven extraction read them.
	// For unstructured prose, pass a single TurnContext with Text
	// populated; the SDK still owns the LLM-visible wire shape.
	Turns []TurnContext

	// ObservedAt anchors the wall-clock for relative-time
	// resolution ("yesterday", "last weekend") inside Turns. When
	// zero the compiler uses time.Now(); historical replay callers
	// MUST set ObservedAt to the conversation's real wall time or
	// relative-time resolution silently drifts to "now".
	ObservedAt time.Time

	// Tier is an optional importance intent label applied to every
	// fact in this save ("core", "general", "data", "storage"). Empty
	// means "general". Tier adjusts Confidence at ingest time; it is
	// not stored on TemporalFact. For per-fact gradients set
	// Confidence on each TemporalFact directly.
	Tier string
}

// SaveResult reports the canonical fact ids that were appended to the
// ledger. Dedupe/policy drops are not listed here; telemetry surfaces
// them via the projection hook.
type SaveResult struct {
	FactIDs []string
}

// TimeRange bounds timeline recall. Aliases domain.TimeRange.
type TimeRange = domain.TimeRange

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
	// GraphHops bounds graph expansion when graph is enabled via
	// WithGraphEnabled. Zero uses the graph projection default.
	GraphHops int

	// Trust applies read-time visibility filtering. Nil disables the
	// trust_filter stage.
	Trust *TrustContext
}

// TimeRangeFrom is a convenience for building a half-open range.
func TimeRangeFrom(from, to time.Time) TimeRange {
	return TimeRange{From: from, To: to}
}

// Hit is a materialized recall result. Score semantics are owned by
// the fusion layer.
// Hit is one recall winner. Score is the fused score after the
// post-materialize ranker has applied its boost. Sources lists every
// CandidateSource that surfaced this fact, in the order fusion saw
// them; consumers can read it to attribute winners to specific
// sources (retrieval / entity / timeline / relation / profile /
// graph) for diagnostics and explainability, or to weight downstream
// rendering by source provenance. An empty Sources slice means the
// candidate carried no provenance metadata (legacy / test-only
// paths); it does not imply the hit is invalid.
type Hit struct {
	Fact    TemporalFact
	Score   float64
	Sources []string
}
