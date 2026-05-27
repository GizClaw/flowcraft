package domain

import "time"

// SaveRequest is the canonical write-input shape. Adapters
// construct a SaveRequest once; the recall facade aliases this type
// directly so callers and tests share a single schema.
//
// Two input channels (Memory.Save consumes both):
//
//  1. Facts — fully-structured TemporalFacts (passthrough path).
//  2. Turns — typed per-turn metadata (id, time, speaker, role,
//     text) for opt-in LLM-backed extractors.
type SaveRequest struct {
	Facts []TemporalFact
	Turns []TurnContext

	// ObservedAt anchors the wall-clock for relative-time
	// resolution inside Turns. Zero means "use time.Now()".
	ObservedAt time.Time

	// Tier is an optional importance intent label ("core" /
	// "general" / "data" / "storage"). Empty → "general". Tier
	// adjusts Confidence at ingest; it is not stored on the Fact.
	Tier string

	// RecentMessages is optional prior-turn context for the LLM extractor.
	RecentMessages []Message

	// ExistingFactsAnchor is optional dedup context for extract.
	ExistingFactsAnchor []TemporalFact

	// Mode controls write semantics. Zero value = synchronous
	// (preserves current behaviour). WriteModeAsyncSemantic stores
	// raw episodes synchronously and enqueues semantic extraction
	// for caller-driven workers (see
	// recall-v2-async-semantic-write.md §3.1).
	Mode WriteMode
}

// TurnContext is the typed, adapter-owned shape of one source turn
// the extractor consumes. Adapters translate their wire format into
// TurnContext once; the SDK owns rendering it to the LLM and
// resolving its fields downstream.
type TurnContext struct {
	ID         string
	EvidenceID string
	SessionID  string
	Role       string
	Speaker    string
	Time       time.Time
	Text       string
}

// Message is one caller-supplied conversational turn for LLM extract prompt
// context. Recall does not fetch history itself; callers compose
// RecentMessages from their own history store.
type Message struct {
	Role    string
	Speaker string
	Text    string
	Time    time.Time
}

// Save-tier intent labels. These are caller-supplied importance hints on
// SaveRequest only; they are not persisted on TemporalFact. Map to Confidence
// adjustments in ingest/salience.go.
const (
	TierCore    = "core"
	TierGeneral = "general"
	TierData    = "data"
	TierStorage = "storage"
)

// NormalizeSaveTier returns the effective tier for an empty or
// unknown input (defaults to general).
func NormalizeSaveTier(tier string) string {
	switch tier {
	case TierCore, TierGeneral, TierData, TierStorage:
		return tier
	default:
		return TierGeneral
	}
}
