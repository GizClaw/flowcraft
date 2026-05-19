package compiler

import (
	"strings"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// EntityResolver maps surface mentions to scope-local canonical entity
// identifiers. The Phase 4 implementation runs an AliasResolver
// over the resolved entity set so caller-supplied alias maps can
// fold e.g. "Bob" -> "robert" without changing the public API.
type EntityResolver interface {
	Resolve(f model.TemporalFact) model.TemporalFact
}

// AliasResolver canonicalises a single surface mention within a
// scope. Implementations stay pure (no I/O); they are consulted
// once per mention per Compile call.
//
// Returning an empty string means "no canonical alias known" and
// the original mention is kept after lower-case normalization.
type AliasResolver interface {
	Canonical(scope model.Scope, mention string) string
}

// NopAliasResolver is the default AliasResolver. It never rewrites
// mentions, so the resolver behaves like PR-2/PR-3.
type NopAliasResolver struct{}

// Canonical implements AliasResolver.
func (NopAliasResolver) Canonical(model.Scope, string) string { return "" }

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
	Aliases map[model.Scope]map[string]string
}

// NewStaticAliasResolver constructs a StaticAliasResolver from a
// per-scope alias map. The map is copied so callers may continue to
// mutate the source without affecting resolved entities.
func NewStaticAliasResolver(in map[model.Scope]map[string]string) *StaticAliasResolver {
	out := make(map[model.Scope]map[string]string, len(in))
	for scope, m := range in {
		lower := make(map[string]string, len(m))
		for alias, canonical := range m {
			lower[strings.ToLower(strings.TrimSpace(alias))] = canonical
		}
		out[scope] = lower
	}
	return &StaticAliasResolver{Aliases: out}
}

// Canonical implements AliasResolver. Resolution falls back through
// progressively-broader scopes: full -> drop AgentID -> drop UserID
// -> runtime-only.
func (r *StaticAliasResolver) Canonical(scope model.Scope, mention string) string {
	if r == nil {
		return ""
	}
	key := strings.ToLower(strings.TrimSpace(mention))
	if key == "" {
		return ""
	}
	for _, s := range fallbackScopes(scope) {
		if m, ok := r.Aliases[s]; ok {
			if v, ok := m[key]; ok {
				return v
			}
		}
	}
	return ""
}

func fallbackScopes(scope model.Scope) []model.Scope {
	out := []model.Scope{scope}
	if scope.AgentID != "" {
		out = append(out, model.Scope{RuntimeID: scope.RuntimeID, UserID: scope.UserID})
	}
	if scope.UserID != "" {
		out = append(out, model.Scope{RuntimeID: scope.RuntimeID})
	}
	return out
}

// aliasEntityResolver implements EntityResolver on top of an
// AliasResolver. Subject / Object mentions, plus all
// Entities/Participants entries, pass through the alias map. The
// final entity set is canonicalised (lower-case, deduped) so the
// projection layer sees stable values.
type aliasEntityResolver struct {
	alias AliasResolver
}

func newAliasEntityResolver(alias AliasResolver) EntityResolver {
	if alias == nil {
		alias = NopAliasResolver{}
	}
	return aliasEntityResolver{alias: alias}
}

// Resolve runs the canonical pipeline:
//  1. Canonicalise Subject / Object via the alias map.
//  2. Add Subject / Object to the entity set.
//  3. Canonicalise the merged entity / participant list.
func (r aliasEntityResolver) Resolve(f model.TemporalFact) model.TemporalFact {
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

func (r aliasEntityResolver) applyAlias(scope model.Scope, mention string) string {
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
