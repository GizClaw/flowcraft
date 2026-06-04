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

	// MetaSubjectSuppressed marks facts whose extractor deliberately
	// rejected the emitted subject as unresolved. Structurizers must not
	// treat an empty subject on these facts as permission to fill it from
	// the supporting turn speaker.
	MetaSubjectSuppressed = "subject_suppressed"

	MetaAssertionFamily             = "assertion_family"
	MetaParameterOwner              = "parameter.owner"
	MetaParameterNamespacePath      = "parameter.namespace_path"
	MetaParameterNameSurface        = "parameter.name_surface"
	MetaParameterCanonicalName      = "parameter.canonical_name"
	MetaParameterOperation          = "parameter.operation"
	MetaParameterValueKind          = "parameter.value_kind"
	MetaParameterRawValue           = "parameter.raw_value"
	MetaParameterNormalizedValue    = "parameter.normalized_value"
	MetaParameterUnit               = "parameter.unit"
	MetaParameterCondition          = "parameter.condition"
	MetaParameterConstraintOperator = "parameter.constraint_operator"
	MetaParameterGroundingLevel     = "parameter.grounding_level"
	MetaParameterSupportSpanIDs     = "parameter.support_span_ids"
	MetaParameterNormalizationTrace = "parameter.normalization_trace"
)
