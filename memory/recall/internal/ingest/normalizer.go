package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/text/normalize"
)

// NopPredicateSynonyms keeps predicates verbatim. Default for PR-4.
type NopPredicateSynonyms struct{}

var _ port.PredicateSynonyms = NopPredicateSynonyms{}

// Canonical implements port.PredicateSynonyms.
func (NopPredicateSynonyms) Canonical(string) string { return "" }

// StaticPredicateSynonyms is a thin map adapter for callers that
// want to declare a fixed synonym table at construction time.
type StaticPredicateSynonyms map[string]string

var _ port.PredicateSynonyms = StaticPredicateSynonyms{}

// Canonical implements port.PredicateSynonyms. Lookups are case
// insensitive on both sides.
func (s StaticPredicateSynonyms) Canonical(predicate string) string {
	if predicate == "" {
		return ""
	}
	key := strings.ToLower(strings.TrimSpace(predicate))
	if v, ok := s[key]; ok {
		return v
	}
	return ""
}

type defaultNormalizer struct {
	synonyms port.PredicateSynonyms
}

var _ port.Normalizer = (*defaultNormalizer)(nil)

// newDefaultNormalizer wires a port.PredicateSynonyms hook. Nil hook
// falls back to NopPredicateSynonyms so PR-3 callers keep working.
func newDefaultNormalizer(syn port.PredicateSynonyms) port.Normalizer {
	if syn == nil {
		syn = NopPredicateSynonyms{}
	}
	return &defaultNormalizer{synonyms: syn}
}

// Normalize harmonises every free-form text column the canonical
// model exposes. The transformations are deterministic and idempotent
// so re-normalising an already-normal fact is a no-op.
func (n *defaultNormalizer) Normalize(f domain.TemporalFact) domain.TemporalFact {
	f.Subject = canonicalSpace(f.Subject)
	f.Object = canonicalSpace(f.Object)
	f.Location = canonicalSpace(f.Location)
	f.Content = canonicalSpace(f.Content)

	pred := canonicalSpace(f.Predicate)
	if pred != "" {
		// predicate is treated as a stable identifier: lowercase +
		// underscore-style normalization so "Favourite Colour" and
		// "favorite_color" merge cleanly when fed through the
		// synonym hook.
		canonical := canonicalSpace(strings.ToLower(normalize.ReplaceNonAlnumWithSpace(pred)))
		if alias := n.synonyms.Canonical(canonical); alias != "" {
			canonical = alias
		}
		f.Predicate = canonical
	}

	f.Entities = canonicalSet(f.Entities)
	f.Participants = canonicalSet(f.Participants)
	f.SourceMessageIDs = uniqueTrimmed(f.SourceMessageIDs)
	return f
}

// canonicalSpace is the package-internal alias for
// [normalize.CollapseSpaces]. Kept as a one-line wrapper so the
// normalizer's call sites stay self-documenting (every fact column
// it touches needs canonical-space form) without leaking the
// sdk/text dependency into the public signature.
func canonicalSpace(s string) string {
	return normalize.CollapseSpaces(s)
}

// canonicalSet trims, lower-cases, and de-duplicates entity-like
// values. The ordering is stable (sorted) so downstream merge keys
// are deterministic regardless of caller order.
func canonicalSet(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func uniqueTrimmed(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// DefaultMergeKey computes the canonical merge key for a fact.
//
// The relation kind is the only shape that materially depends on the
// object (docs §5.5): without it, "Alice spouse Bob" and "Alice
// spouse Carol" would collapse incorrectly. Other kinds key off
// (kind, subject, predicate, normalized content) so semantically
// identical updates dedupe deterministically.
func DefaultMergeKey(f domain.TemporalFact) string {
	subject := strings.ToLower(f.Subject)
	predicate := strings.ToLower(f.Predicate)
	object := strings.ToLower(f.Object)

	switch f.Kind {
	case domain.KindRelation:
		return joinKey("relation", subject, predicate, object)
	case domain.KindState, domain.KindPreference, domain.KindProcedure:
		if subject != "" && predicate != "" {
			return joinKey(string(f.Kind), subject, predicate)
		}
		return joinKey(string(f.Kind), contentDigest(f))
	case domain.KindEvent, domain.KindPlan:
		return joinKey(string(f.Kind), subject, predicate, contentDigest(f))
	case domain.KindNote:
		return joinKey(string(f.Kind), contentDigest(f))
	}
	return joinKey(string(f.Kind), contentDigest(f))
}

func joinKey(parts ...string) string {
	cleaned := make([]string, 0, len(parts))
	for _, p := range parts {
		cleaned = append(cleaned, strings.ReplaceAll(p, "|", "_"))
	}
	return strings.Join(cleaned, "|")
}

func contentDigest(f domain.TemporalFact) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(f.Content))))
	return hex.EncodeToString(h[:8])
}
