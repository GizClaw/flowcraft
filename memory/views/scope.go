package views

import (
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Scope is the shared memory boundary carried by derived views and projections.
//
// RuntimeID and UserID define the hard partition. Empty UserID means global.
// AgentID and evidence/view dimensions are soft filters layered on top.
type Scope struct {
	RuntimeID      string
	UserID         string
	AgentID        string
	ConversationID string
	DatasetID      string
	EntityID       string
}

// Validate checks the minimal invariant required to use a scope as a memory
// boundary. UserID may be empty to represent global memory.
func (s Scope) Validate() error {
	if strings.TrimSpace(s.RuntimeID) == "" {
		return errdefs.Validationf("memory/views: scope runtime_id is required")
	}
	return nil
}

// HardPartitionKey returns the stable runtime/user partition key.
func (s Scope) HardPartitionKey() string {
	return strings.TrimSpace(s.RuntimeID) + "\x00" + strings.TrimSpace(s.UserID)
}

// IsZero reports whether no scope fields were provided.
func (s Scope) IsZero() bool {
	return s == Scope{}
}
