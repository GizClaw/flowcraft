package policy

import (
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// Limits are the session-table decisions an agent makes: when an idle session is
// evicted and how many it may hold at once.
//
// They are part of a Grant because they are parsed from the same document, but
// they are not permissions and they do not travel into a session: [Grant.Narrow]
// clears them, and the digest ignores them. A session does not limit itself.
type Limits struct {
	// Idle is how long a session may sit unused before it is evicted. Zero
	// means the host's default.
	Idle Duration `json:"idle,omitempty" yaml:"idle,omitempty"`
	// MaxSessions caps concurrent sessions. Zero means unbounded.
	MaxSessions int `json:"max_sessions,omitempty" yaml:"max_sessions,omitempty"`
}

// Validate rejects a limit that cannot mean anything.
func (l Limits) Validate() error {
	if l.Idle.Std() < 0 {
		return errdefs.Validationf("policy.limits: idle must not be negative")
	}
	if l.MaxSessions < 0 {
		return errdefs.Validationf("policy.limits: max_sessions must not be negative")
	}
	return nil
}

// IsZero reports whether the limits are unset.
func (l Limits) IsZero() bool { return l == Limits{} }
