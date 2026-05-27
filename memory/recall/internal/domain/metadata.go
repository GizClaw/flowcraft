package domain

// Reserved metadata keys written by canonical projections.
//
// These names are owned by sdk/recall; user-supplied Metadata MUST NOT
// overwrite them. The retrieval projection enforces this by writing
// reserved keys after copying user metadata.
const (
	MetaFactID     = "fact_id"
	MetaFactKind   = "fact_kind"
	MetaScopeRT    = "scope_runtime_id"
	MetaScopeUser  = "scope_user_id"
	MetaScopeAgent = "scope_agent_id"

	MetaMergeKey     = "merge_key"
	MetaSupersededBy = "superseded_by"
	MetaCorrectedBy  = "corrected_by"

	MetaObservedAt = "observed_at"
	MetaValidFrom  = "valid_from"
	MetaValidTo    = "valid_to"

	MetaConfidence = "confidence"
	MetaEntities   = "entities"

	// MetaSensitivity is the write-path sensitivity label stamped by
	// governance (public / internal / private / secret). policy_filter
	// compares it against Query.Trust.MaxSensitivity.
	MetaSensitivity = "sensitivity"

	// Revision metadata.
	MetaRevisionKind = "revision_kind"
	MetaForkOf       = "fork_of"
	MetaContestOf    = "contest_of"

	// Feedback fields mirrored into retrieval metadata.
	MetaReinforcement = "reinforcement"
	MetaPenalty       = "penalty"

	// MetaCoverageRepair marks facts emitted by extractor coverage
	// repair. These facts are grounded in source turns but were
	// produced by a narrower second-pass extraction, so downstream
	// rankers can treat them as useful but lower-confidence evidence
	// when they compete with normal first-pass memories.
	MetaCoverageRepair = "coverage_repair"
)
