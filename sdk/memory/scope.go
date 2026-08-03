package memory

import (
	"errors"
	"fmt"
	"strings"
)

// Scope is the addressing key for every memory operation. It is the
// kernel ↔ memory contract for partitioning: the hard fields
// (RuntimeID, UserID) fix which physical record an operation must
// touch, and the soft fields (AgentID, ConversationID, DatasetID)
// are hints that an implementation may use to filter or bucket
// without ever crossing the hard partition.
//
// RuntimeID is required; an empty RuntimeID is a programming error.
// UserID is the tenant boundary: when empty the Scope is "global"
// (single-tenant or test) — this is documented behaviour, not the
// zero value, so callers must construct the Scope explicitly.
type Scope struct {
	// RuntimeID is the hard partition that names the memory
	// instance. Two operations that should land on the same
	// physical store must agree on RuntimeID.
	RuntimeID string
	// UserID is the hard tenant partition. Empty selects the
	// global scope, which the runtime treats as a documented
	// override of the natural zero value.
	UserID string
	// AgentID is a soft filter an implementation may use for
	// recall scoring or storage bucketing. Never widens the
	// hard partition.
	AgentID string
	// ConversationID is a soft filter. Together with the hard
	// partition it identifies a single transcript.
	ConversationID string
	// DatasetID is a soft filter that scopes a document
	// collection (Import / Recall against knowledge).
	DatasetID string
}

// Validate enforces the documented boundary: a Scope must have a
// non-empty RuntimeID. Empty UserID is allowed and means "global".
func (s Scope) Validate() error {
	if s.RuntimeID == "" {
		return newError(KindScopeInvalid, "", "", errors.New("memory: Scope.RuntimeID is required"))
	}
	if strings.ContainsRune(s.RuntimeID, '\x00') {
		return newError(KindScopeInvalid, "", "", errors.New("memory: Scope.RuntimeID must not contain NUL"))
	}
	if strings.ContainsRune(s.UserID, '\x00') {
		return newError(KindScopeInvalid, "", "", errors.New("memory: Scope.UserID must not contain NUL"))
	}
	return nil
}

// HardPartitionKey returns the canonical key under which the
// runtime must project the Scope onto a physical record. The NUL
// separator is not a valid byte in any documented ID field, so
// concatenating RuntimeID and UserID with it cannot collide.
func (s Scope) HardPartitionKey() string {
	return s.RuntimeID + "\x00" + s.UserID
}

// IsZero reports whether the Scope is the zero value. A zero Scope
// is invalid (RuntimeID is empty); callers should Validate before
// using it.
func (s Scope) IsZero() bool {
	return s.RuntimeID == "" &&
		s.UserID == "" &&
		s.AgentID == "" &&
		s.ConversationID == "" &&
		s.DatasetID == ""
}

// String renders a Scope in the form used by logs and telemetry.
// It is for human consumption and is not a stable contract.
func (s Scope) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "rt=%s", s.RuntimeID)
	if s.UserID != "" {
		fmt.Fprintf(&b, ", user=%s", s.UserID)
	}
	if s.AgentID != "" {
		fmt.Fprintf(&b, ", agent=%s", s.AgentID)
	}
	if s.ConversationID != "" {
		fmt.Fprintf(&b, ", conv=%s", s.ConversationID)
	}
	if s.DatasetID != "" {
		fmt.Fprintf(&b, ", ds=%s", s.DatasetID)
	}
	return b.String()
}
