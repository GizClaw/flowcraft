package model

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
)
