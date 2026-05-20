package ingest

import (
	"strings"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
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

// aliasEntityResolver implements EntityResolver on top of an
// AliasResolver. Subject / Object mentions, plus all
// Entities/Participants entries, pass through the alias map. The
// final entity set is canonicalised (lower-case, deduped) so the
// projection layer sees stable values.
type aliasEntityResolver struct {
	alias port.AliasResolver
}

var _ port.EntityResolver = aliasEntityResolver{}

func newAliasEntityResolver(alias port.AliasResolver) port.EntityResolver {
	if alias == nil {
		alias = NopAliasResolver{}
	}
	return aliasEntityResolver{alias: alias}
}

// Resolve runs the canonical pipeline:
//  1. Canonicalise Subject / Object via the alias map.
//  2. Add Subject / Object to the entity set.
//  3. Canonicalise the merged entity / participant list.
func (r aliasEntityResolver) Resolve(f domain.TemporalFact) domain.TemporalFact {
	f.Subject = r.applyAlias(f.Scope, f.Subject)
	f.Object = r.applyAlias(f.Scope, f.Object)

	seen := make(map[string]struct{}, len(f.Entities))
	out := make([]string, 0, len(f.Entities)+2)
	add := func(s string) {
		canon := canonicalEntityLowercase(r.applyAlias(f.Scope, s))
		if canon == "" {
			return
		}
		if _, ok := seen[canon]; ok {
			return
		}
		seen[canon] = struct{}{}
		out = append(out, canon)
	}
	for _, e := range f.Entities {
		add(e)
	}
	add(f.Subject)
	add(f.Object)
	f.Entities = out

	// Participants gets the same alias treatment but stays
	// independent of the entity set so callers can distinguish
	// "mentioned" from "participated in".
	if len(f.Participants) > 0 {
		seenP := make(map[string]struct{}, len(f.Participants))
		outP := make([]string, 0, len(f.Participants))
		for _, p := range f.Participants {
			canon := canonicalEntityLowercase(r.applyAlias(f.Scope, p))
			if canon == "" {
				continue
			}
			if _, ok := seenP[canon]; ok {
				continue
			}
			seenP[canon] = struct{}{}
			outP = append(outP, canon)
		}
		f.Participants = outP
	}
	return f
}

func (r aliasEntityResolver) applyAlias(scope domain.Scope, mention string) string {
	if mention == "" {
		return ""
	}
	if canonical := r.alias.Canonical(scope, mention); canonical != "" {
		return canonical
	}
	return mention
}

func canonicalEntityLowercase(s string) string {
	out := canonicalSpace(s)
	if out == "" {
		return ""
	}
	return strings.ToLower(out)
}
