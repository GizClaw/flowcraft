package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/GizClaw/flowcraft/core/model"
)

// digestView is the part of a Grant that decides what a session may do: every field of one.
//
// It is a separate type so that the two field sets are checked against each other. A field added to
// Grant without one here does not compile, which is the decision being made deliberately rather
// than a new setting silently entering — or silently missing — a session's identity.
// Limits are deliberately absent: they are the session table's policy, and
// changing an idle timeout must not invalidate a checkpoint.
type digestView struct {
	Sandbox    string           `json:"sandbox"`
	Files      Files            `json:"files"`
	Net        NetPolicy        `json:"net"`
	Exec       ExecPolicy       `json:"exec"`
	Resources  Resources        `json:"resources"`
	Models     []model.ModelRef `json:"models"`
	Tools      ToolSet          `json:"tools"`
	Delegation *Delegation      `json:"delegation"`
}

// Digest is the stable identity of a permission set: the value a span renders as
// session.bind.id, and the value an approval records on both sides of a widening.
//
// Only the normalized form is hashed, so two spellings of the same permission —
// ["/srv", "/srv/work"] and ["/srv"], or ["/"] and omitted — produce the same id.
// A grant that fails validation is hashed as given: validation happens at load
// time, and a digest is a diagnostic, not a gate.
func (g Grant) Digest() string {
	normalized, err := g.Normalized()
	if err != nil {
		normalized = g
	}
	payload, err := json.Marshal(digestView(normalized))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:16])
}
