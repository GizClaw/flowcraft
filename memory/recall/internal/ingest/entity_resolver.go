package ingest

import (
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

// RuleBasedEntityExtractor is the default deterministic
// port.EntityExtractor. It combines two rule families:
//
//  1. Title-Cased token NER over the content (heuristic English
//     proper-noun detection — see extractEntities in structurizer.go
//     for the exact stopword + tokenisation rules).
//  2. Substring matching against KnownEntities' canonical and alias
//     surface forms so previously-seen entities get folded back into
//     the fact even when the LLM did not Title-Case them.
//
// Known limitations (the reason this is a swappable interface):
//   - English-centric: capitalisation is the primary NER signal,
//     so non-Latin scripts and lower-case proper nouns are missed.
//   - No disambiguation: two different "Bob"s collapse to one
//     canonical entity.
//   - No multi-word phrase detection beyond alias matching.
//
// Production deployments needing non-English content or entity
// disambiguation should plug in a custom EntityExtractor that calls
// an external NER service (spaCy, an LLM, a cross-encoder).
type RuleBasedEntityExtractor struct{}

var _ port.EntityExtractor = RuleBasedEntityExtractor{}

// ExtractEntities implements port.EntityExtractor.
func (RuleBasedEntityExtractor) ExtractEntities(content string, known []port.EntitySnapshot) []string {
	return extractEntities(content, known)
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
		if canon == "" || isWeakExtractedEntity(canon) {
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
