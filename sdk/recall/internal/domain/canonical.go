package domain

import "strings"

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

// ScopesMatch reports whether two scopes share the same primary
// partition keys (ignores Federation, which is recall-only).
func ScopesMatch(a, b Scope) bool {
	return a.CanonicalKey() == b.CanonicalKey()
}
