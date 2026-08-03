package memory

// Operation names a root memory operation. It is part of the typed
// contract: telemetry, error routing, and CompileResult all carry
// the same name space.
type Operation string

const (
	OpAppend  Operation = "append"
	OpLoad    Operation = "load"
	OpRecall  Operation = "recall"
	OpImport  Operation = "import"
	OpCompact Operation = "compact"
	OpArchive Operation = "archive"
)

// FieldID names one canonical field of a Request in the compile
// ledger. The set is intentionally small and stable: every op
// exposes a fixed list of FieldIDs that callers can rely on, and
// compilers must account for each one with a Decision.
type FieldID string

const (
	// Append fields.
	FieldAppendScope          FieldID = "append.scope"
	FieldAppendConversationID FieldID = "append.conversation_id"
	FieldAppendIdempotencyKey FieldID = "append.idempotency_key"
	FieldAppendRecords        FieldID = "append.records"
	FieldAppendMetadata       FieldID = "append.metadata"

	// Load fields.
	FieldLoadScope          FieldID = "load.scope"
	FieldLoadConversationID FieldID = "load.conversation_id"
	FieldLoadCursor         FieldID = "load.cursor"
	FieldLoadLimit          FieldID = "load.limit"
	FieldLoadReverse        FieldID = "load.reverse"

	// Recall fields.
	FieldRecallScope          FieldID = "recall.scope"
	FieldRecallConversationID FieldID = "recall.conversation_id"
	FieldRecallQuery          FieldID = "recall.query"
	FieldRecallTopK           FieldID = "recall.top_k"
	FieldRecallFilters        FieldID = "recall.filters"
	FieldRecallMinScore       FieldID = "recall.min_score"

	// Import fields.
	FieldImportScope       FieldID = "import.scope"
	FieldImportDatasetID   FieldID = "import.dataset_id"
	FieldImportSource      FieldID = "import.source"
	FieldImportTags        FieldID = "import.tags"
	FieldImportChunkPolicy FieldID = "import.chunk_policy"

	// Compact fields.
	FieldCompactScope     FieldID = "compact.scope"
	FieldCompactOlderThan FieldID = "compact.older_than"
	FieldCompactKeep      FieldID = "compact.keep"

	// Archive fields.
	FieldArchiveScope       FieldID = "archive.scope"
	FieldArchiveOlderThan   FieldID = "archive.older_than"
	FieldArchiveDestination FieldID = "archive.destination"
)

// Disposition says what the compiler decided to do with a field.
type Disposition string

const (
	// DispositionNative means the compiler consumed the field
	// into the implementation's wire. The runtime will pass
	// the value through.
	DispositionNative Disposition = "native"
	// DispositionRejected means the field cannot be honoured
	// and Execute MUST refuse the request. The Reason is
	// stable for diagnostics.
	DispositionRejected Disposition = "rejected"
)

// Reason is a short stable code that explains a Rejected Decision.
// Concrete implementations may extend this set; the constants here
// are the ones the kernel knows how to map to ErrorKind.
type Reason string

const (
	// ReasonUnsupportedFeature — the implementation does not
	// implement the feature the field asks for.
	ReasonUnsupportedFeature Reason = "unsupported_feature"
	// ReasonInvalidExtension — the field carries an unknown
	// or conflicting extension.
	ReasonInvalidExtension Reason = "invalid_extension"
	// ReasonInvalidValue — the field value is malformed
	// (negative Limit, malformed Scope, etc.).
	ReasonInvalidValue Reason = "invalid_value"
	// ReasonNotConfigured — the implementation is missing
	// for this op.
	ReasonNotConfigured Reason = "not_configured"
	// ReasonPolicyDenied — the field is forbidden by hook
	// policy.
	ReasonPolicyDenied Reason = "policy_denied"
	// ReasonScopeInvalid — the request scope is malformed or targets a
	// different runtime partition.
	ReasonScopeInvalid Reason = "scope_invalid"
)

// Decision is the compiler's verdict on a single canonical field.
type Decision struct {
	Field       FieldID     `json:"field"`
	Disposition Disposition `json:"disposition"`
	Reason      Reason      `json:"reason,omitempty"`
	Message     string      `json:"message,omitempty"`
}

// CompileResult is what every Compile method returns. It is the
// only authority on whether a request is executable. The runtime
// validates the ledger before Execute and refuses requests with
// missing or duplicate decisions.
type CompileResult struct {
	Op        Operation
	Decisions []Decision
}

// AllNative reports whether every decision in the ledger is Native.
// A nil or empty ledger is NOT considered all-Native: the runtime
// requires a complete ledger, so a missing decision is an internal
// error.
func (r CompileResult) AllNative() bool {
	if len(r.Decisions) == 0 {
		return false
	}
	for _, d := range r.Decisions {
		if d.Disposition != DispositionNative {
			return false
		}
	}
	return true
}

// Rejected returns the first Rejected Decision, or the zero value
// when the ledger contains no Rejection.
func (r CompileResult) Rejected() (Decision, bool) {
	for _, d := range r.Decisions {
		if d.Disposition == DispositionRejected {
			return d, true
		}
	}
	return Decision{}, false
}

// HasField reports whether the ledger contains a decision for the
// given canonical field. The runtime uses it to enforce the ledger
// is complete before Execute.
func (r CompileResult) HasField(field FieldID) bool {
	for _, d := range r.Decisions {
		if d.Field == field {
			return true
		}
	}
	return false
}

// Clone returns a deep copy of the result. The compile ledger is
// caller-visible; cloning keeps callers from mutating shared state.
func (r CompileResult) Clone() CompileResult {
	if r.Decisions == nil {
		return CompileResult{Op: r.Op}
	}
	out := make([]Decision, len(r.Decisions))
	copy(out, r.Decisions)
	return CompileResult{Op: r.Op, Decisions: out}
}

// nativeDecision is a small constructor for the common Native
// case. Compile implementations build their ledger with it.
func nativeDecision(field FieldID) Decision {
	return Decision{Field: field, Disposition: DispositionNative}
}

// rejectedDecision is a small constructor for the Rejected case.
// The Message is for humans; Reason is the stable code.
func rejectedDecision(field FieldID, reason Reason, message string) Decision {
	return Decision{
		Field:       field,
		Disposition: DispositionRejected,
		Reason:      reason,
		Message:     message,
	}
}
