package compiler

import "github.com/GizClaw/flowcraft/sdk/recall/internal/model"

// EntityResolver maps surface mentions to scope-local canonical entity
// identifiers. Phase 1 ships a passthrough; Phase 4 layers in alias
// maps and conflict explanations.
type EntityResolver interface {
	Resolve(f model.TemporalFact) model.TemporalFact
}

type passthroughEntityResolver struct{}

// Resolve relies on the normalizer's canonical_set treatment of
// Entities / Participants and only ensures Subject/Object are added
// to the entity set so projections downstream can index them.
func (passthroughEntityResolver) Resolve(f model.TemporalFact) model.TemporalFact {
	seen := make(map[string]struct{}, len(f.Entities))
	for _, e := range f.Entities {
		seen[e] = struct{}{}
	}
	for _, candidate := range []string{f.Subject, f.Object} {
		if candidate == "" {
			continue
		}
		key := lowerCanonical(candidate)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		f.Entities = append(f.Entities, key)
	}
	return f
}

func lowerCanonical(s string) string {
	return canonicalSpaceLower(s)
}

func canonicalSpaceLower(s string) string {
	out := canonicalSpace(s)
	if out == "" {
		return ""
	}
	low := make([]byte, len(out))
	for i := 0; i < len(out); i++ {
		c := out[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		low[i] = c
	}
	return string(low)
}
