package domain

import "strings"

// Scope identifies the tenant/user partition for canonical memory.
//
// RuntimeID and UserID participate in storage / namespace partitioning;
// AgentID is a soft-isolation dimension surfaced through metadata, not
// through partitioning, so a single agent can union its own facts with
// shared ones during recall.
//
// Federation is read-path only: Save / Forget / revision APIs use the
// primary scope and ignore Federation (write path does not federate).
// Federation lists additional sub-scopes to recall from; only one
// level is expanded — sub-scope Federation fields are ignored.
//
// v1 Partition translation:
//
//	Partitions:[User, Global] on scope {RuntimeID: rt, UserID: alice}
//	≈ Federation: []Scope{{RuntimeID: rt}} on the same primary scope.
type Scope struct {
	RuntimeID string
	AgentID   string
	UserID    string

	// Federation lists extra scopes to include on Recall. nil and
	// empty slice are equivalent (primary scope only).
	Federation []Scope
}

// CanonicalKey returns a stable scope qualifier for federation dedup
// and ForgetAll confirmation ("rt-1/u:alice" / "rt-1/global").
func (s Scope) CanonicalKey() string {
	if s.RuntimeID == "" {
		return ""
	}
	if s.UserID == "" {
		return s.RuntimeID + "/global"
	}
	key := s.RuntimeID + "/u:" + s.UserID
	if a := strings.TrimSpace(s.AgentID); a != "" {
		key += "/a:" + a
	}
	return key
}

// EffectiveFederation returns the deduped recall scope list: primary
// first, then each Federation entry (sub-scope Federation ignored).
func (s Scope) EffectiveFederation() []Scope {
	primary := Scope{
		RuntimeID: s.RuntimeID,
		UserID:    s.UserID,
		AgentID:   s.AgentID,
	}
	seen := map[string]struct{}{primary.CanonicalKey(): {}}
	out := []Scope{primary}
	for _, sub := range s.Federation {
		normalized := Scope{
			RuntimeID: sub.RuntimeID,
			UserID:    sub.UserID,
			AgentID:   sub.AgentID,
		}
		if normalized.RuntimeID == "" {
			continue
		}
		k := normalized.CanonicalKey()
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

// FactKind classifies a canonical memory fact. The enum is closed; see
// docs §5.3 for projection eligibility per kind.
type FactKind string

const (
	KindEvent      FactKind = "event"
	KindState      FactKind = "state"
	KindPreference FactKind = "preference"
	KindRelation   FactKind = "relation"
	KindPlan       FactKind = "plan"
	KindNote       FactKind = "note"
)

// ScopesMatch reports whether two scopes share the same primary
// partition keys (ignores Federation, which is recall-only).
func ScopesMatch(a, b Scope) bool {
	return a.CanonicalKey() == b.CanonicalKey()
}

// IsValid reports whether k is one of the canonical FactKinds.
func (k FactKind) IsValid() bool {
	switch k {
	case KindEvent, KindState, KindPreference, KindRelation, KindPlan, KindNote:
		return true
	}
	return false
}
