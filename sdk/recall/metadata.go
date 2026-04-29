package recall

import "github.com/GizClaw/flowcraft/sdk/retrieval"

// Reserved metadata keys written by the recall package and read by
// both the recall package itself and the retrieval pipeline stages
// (e.g. SupersededDecay reads MetaSupersededBy).
//
// External callers SHOULD treat these keys as reserved: setting them
// via Entry.Metadata or a custom extractor will be silently
// overwritten by the Save path, and any pre-existing user data stored
// under MetaTombstone will be hidden from Recall (use
// Request.WithTombstoned to opt out, see [Request]).
//
// The slot key constant lives in the retrieval package
// ([retrieval.MetaSlotKey]) so SlotCollapse and SlotKeyOf can read it
// without importing recall and creating a cycle.
const (
	// MetaSubject stores the canonical subject of a slot fact (after
	// alias normalisation). Written only when both Subject and
	// Predicate are non-empty AND neither contains the slot delimiter
	// '|'. See upsertFacts.
	MetaSubject = "subject"

	// MetaPredicate stores the canonical predicate of a slot fact.
	// Same write conditions as MetaSubject.
	MetaPredicate = "predicate"

	// MetaSupersededBy is the entry ID that replaced this entry. Set
	// by all three supersede channels (slot / vector / resolver).
	// Pipeline.SupersededDecay damps any hit carrying this key at
	// recall time.
	MetaSupersededBy = "superseded_by"

	// MetaSupersededAt is the unix-millis timestamp of the supersede
	// action; co-written with MetaSupersededBy.
	MetaSupersededAt = "superseded_at"

	// MetaTombstone is set to bool(true) by the OpDelete branch of
	// the LLM update resolver. Recall composes [TombstoneFilter]
	// unconditionally to hide tombstoned entries; callers that need
	// to see them must pass Request.WithTombstoned = true.
	MetaTombstone = "tombstone"

	// MetaContentHash is the per-namespace MD5 of the entry content,
	// scoped by UserID. Used by the dedup probe at Save time.
	MetaContentHash = "content_hash"

	// MetaSourceLabel mirrors ExtractedFact.Source so post-hoc
	// analytics can group entries by extractor provenance.
	MetaSourceLabel = "source_label"

	// MetaEntities is the list of named entities (people, places,
	// products, …) the extractor attached to the fact. Stored as
	// []string but tolerated as []any for adapter round-trips.
	MetaEntities = "entities"
)

// MetaSlotKey is re-exported from the retrieval package so recall
// callers can stay within one import. See [retrieval.MetaSlotKey] for
// the underlying contract.
const MetaSlotKey = retrieval.MetaSlotKey

// slotDelimiter separates subject and predicate inside MetaSlotKey.
// Subjects/predicates that contain this byte are NOT eligible for the
// slot supersede channel (they would create ambiguous keys); the Save
// path drops the slot fields entirely in that case so the fact
// degrades to the vector / resolver supersede channels instead.
const slotDelimiter = "|"
