package session

import (
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Key identifies one conversational session. AgentID is part of the identity:
// two agents using the same ContextID do not share session state.
type Key struct {
	AgentID   string
	ContextID string
}

// Validate rejects empty, whitespace-only, and padded identifiers.
func (k Key) Validate() error {
	if k.AgentID == "" || strings.TrimSpace(k.AgentID) != k.AgentID {
		return errdefs.Validationf("runtime session: AgentID must be non-empty and must not have surrounding whitespace")
	}
	if k.ContextID == "" || strings.TrimSpace(k.ContextID) != k.ContextID {
		return errdefs.Validationf("runtime session: ContextID must be non-empty and must not have surrounding whitespace")
	}
	return nil
}
