package ingest

import (
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// NopAliasResolver is the default port.AliasResolver. It never rewrites
// mentions, so the resolver behaves like PR-2/PR-3.
type NopAliasResolver struct{}

var _ port.AliasResolver = NopAliasResolver{}

// Canonical implements port.AliasResolver.
func (NopAliasResolver) Canonical(domain.Scope, string) string { return "" }

// StaticAliasResolver maps surface aliases to canonical mention
// strings on a per-scope basis. Lookups are case-insensitive on the
// alias side; canonical values are returned verbatim.
//
// Scope key: when the user constructs aliases for a specific
// (runtime, user, agent) triple, they win over (runtime, user)
// fallback, which in turn wins over (runtime) fallback.
type StaticAliasResolver struct {
	// Aliases maps Scope -> alias -> canonical. Implementations
	// are encouraged to construct it via NewStaticAliasResolver
	// instead of populating directly so lookups stay
	// case-insensitive.
	// Aliases keys are Scope.CanonicalKey() strings (Scope is not
	// comparable once Federation []Scope is present).
	Aliases map[string]map[string]string
}

// ScopeAliasEntry binds aliases to a scope without using Scope as a
// map key (Scope contains a slice and is not comparable).
type ScopeAliasEntry struct {
	Scope   domain.Scope
	Aliases map[string]string
}

// NewStaticAliasResolver constructs a StaticAliasResolver from
// per-scope alias entries.
func NewStaticAliasResolver(entries ...ScopeAliasEntry) *StaticAliasResolver {
	out := make(map[string]map[string]string, len(entries))
	for _, e := range entries {
		lower := make(map[string]string, len(e.Aliases))
		for alias, canonical := range e.Aliases {
			lower[strings.ToLower(strings.TrimSpace(alias))] = canonical
		}
		out[e.Scope.CanonicalKey()] = lower
	}
	return &StaticAliasResolver{Aliases: out}
}

var _ port.AliasResolver = (*StaticAliasResolver)(nil)

// Canonical implements port.AliasResolver. Resolution falls back through
// progressively-broader scopes: full -> drop AgentID -> drop UserID
// -> runtime-only.
func (r *StaticAliasResolver) Canonical(scope domain.Scope, mention string) string {
	if r == nil {
		return ""
	}
	key := strings.ToLower(strings.TrimSpace(mention))
	if key == "" {
		return ""
	}
	for _, s := range fallbackScopes(scope) {
		if m, ok := r.Aliases[s.CanonicalKey()]; ok {
			if v, ok := m[key]; ok {
				return v
			}
		}
	}
	return ""
}

func fallbackScopes(scope domain.Scope) []domain.Scope {
	out := []domain.Scope{scope}
	if scope.AgentID != "" {
		out = append(out, domain.Scope{RuntimeID: scope.RuntimeID, UserID: scope.UserID})
	}
	if scope.UserID != "" {
		out = append(out, domain.Scope{RuntimeID: scope.RuntimeID})
	}
	return out
}
