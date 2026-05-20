package compiler

import "time"

// TurnContext is the typed, adapter-owned shape of one source turn
// the extractor consumes. Adapters translate their wire format into
// TurnContext once; the SDK owns rendering it to the LLM and
// resolving its fields downstream.
//
// Compared to a free-form text path:
//
//   - Time and Speaker are typed channels: the LLM never has to grep
//     timestamps out of prose or guess whether "Tom:" is a speaker
//     or a prose colon. The Structurizer reads these fields directly
//     to ground temporal arithmetic and Subject inference.
//   - ID is the canonical supporting-turn id the model should cite in
//     evidence_refs; the SDK pre-fills it so the model only has to
//     copy the string back, never invent a value.
//   - Role is kept for backward compatibility with conversational
//     prompts; Speaker is preferred when both are present.
//
// All fields except ID and Text are optional. Adapters without
// per-turn timestamps still pass through cleanly with Time zero.
type TurnContext struct {
	// ID is the canonical supporting-turn id the LLM cites in
	// evidence_refs[].id and source_message_ids. Required.
	ID string
	// EvidenceID is the optional adapter-specific evidence id used
	// by downstream consumers (e.g. evaluation harnesses) to match
	// hits back to ground-truth supporting turns. When non-empty
	// the SDK uses it as the LLM-visible identity; ID stays the
	// internal handle.
	EvidenceID string
	// SessionID groups turns that share a conversational session
	// boundary. Adapters that segment by session populate this so
	// downstream rendering can show / cite session context.
	SessionID string
	// Role is "user" / "assistant" / "system" etc. Optional.
	Role string
	// Speaker is the canonical human / agent name. Preferred over
	// Role when filling Subject / Participants. Optional.
	Speaker string
	// Time is the typed absolute timestamp of the turn. Zero means
	// "no typed timestamp" and the Structurizer falls back to
	// regex / observed-at heuristics for relative grounding.
	Time time.Time
	// Text is the body of the turn, with any "[time] speaker:"
	// prefix already stripped by the adapter.
	Text string
}

// EntitySnapshot is a hint about an entity the canonical projection
// has already seen in this scope. The compiler uses these snapshots
// to (a) deduplicate freshly-extracted entities against historical
// canonical forms so "tom" / "Tom" / "Tom Smith" don't fragment the
// graph, and (b) seed the Structurizer's NER pass with high-confidence
// entity matches.
//
// Adapters / callers populate snapshots from the entity projection's
// Lookup / TopMentions; the compiler treats them as a soft hint —
// missing / outdated snapshots only mean less canonicalization, not
// extraction failure.
type EntitySnapshot struct {
	// Canonical is the lowercased / trimmed canonical form. The
	// retrieval / entity projections both index on this form.
	Canonical string
	// Aliases are surface forms previously seen for the canonical
	// entity (capitalised variants, nicknames, etc.). Used by the
	// Structurizer to match unstructured prose back to canonical.
	Aliases []string
}
