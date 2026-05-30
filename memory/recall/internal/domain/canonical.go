package domain

import "strings"

// PartitionKey is the canonical store / projection / async-queue
// partition ("rt-1/u:alice" / "rt-1/global"). AgentID is intentionally
// excluded — per docs §5.1 it is soft isolation, not a hard shard.
// ForgetAll(Hard) confirmScopeKey and queue CancelScope / PurgeScope
// MUST use this key so an agent-scoped Scope cannot confirm a wipe
// that deletes sibling agents' ledger rows in the same user partition.
func (s Scope) PartitionKey() string {
	if s.RuntimeID == "" {
		return ""
	}
	if s.UserID == "" {
		return s.RuntimeID + "/global"
	}
	return s.RuntimeID + "/u:" + s.UserID
}

// CanonicalKey returns a stable scope qualifier for federation dedup
// and sub-scope identity. It extends PartitionKey with /a:{agent}
// when AgentID is set.
func (s Scope) CanonicalKey() string {
	key := s.PartitionKey()
	if key == "" {
		return ""
	}
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

// ScopesMatch reports whether two scopes share the same canonical
// store partition (runtime + user). Federation and AgentID are
// recall-only dimensions and do not affect the match.
func ScopesMatch(a, b Scope) bool {
	return a.PartitionKey() == b.PartitionKey()
}
