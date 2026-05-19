package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Normalizer canonicalizes free-form fields so deterministic merge
// keys produce stable identities. The Phase 4 default applies
// Unicode NFC, strips ASCII punctuation that doesn't contribute to
// semantics, and runs an optional PredicateSynonyms hook over the
// predicate column so callers can fold "favorite_color" /
// "favourite-colour" into a single canonical token.
type Normalizer interface {
	Normalize(f model.TemporalFact) model.TemporalFact
}

// PredicateSynonyms maps any equivalent predicate spellings to a
// single canonical form. It runs after whitespace/case
// normalization, so map keys should already be lower-case
// canonical-spaced. The hook is consulted with the post-hardening
// predicate; returning the empty string means "no synonym known"
// and the original predicate is preserved.
type PredicateSynonyms interface {
	Canonical(predicate string) string
}

// NopPredicateSynonyms keeps predicates verbatim. Default for PR-4.
type NopPredicateSynonyms struct{}

// Canonical implements PredicateSynonyms.
func (NopPredicateSynonyms) Canonical(string) string { return "" }

// StaticPredicateSynonyms is a thin map adapter for callers that
// want to declare a fixed synonym table at construction time.
type StaticPredicateSynonyms map[string]string

// Canonical implements PredicateSynonyms. Lookups are case
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
	synonyms PredicateSynonyms
}

// newDefaultNormalizer wires a PredicateSynonyms hook. Nil hook
// falls back to NopPredicateSynonyms so PR-3 callers keep working.
func newDefaultNormalizer(syn PredicateSynonyms) Normalizer {
	if syn == nil {
		syn = NopPredicateSynonyms{}
	}
	return &defaultNormalizer{synonyms: syn}
}

// Normalize harmonises every free-form text column the canonical
// model exposes. The transformations are deterministic and idempotent
// so re-normalising an already-normal fact is a no-op.
func (n *defaultNormalizer) Normalize(f model.TemporalFact) model.TemporalFact {
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
		canonical := strings.ToLower(replacePunctuationWithSpace(pred))
		canonical = canonicalSpace(canonical)
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

// canonicalSpace folds Unicode-NFC + collapses internal whitespace
// to single ASCII space + trims edge whitespace. Output is suitable
// for direct equality comparison and stable hashing.
func canonicalSpace(s string) string {
	if s == "" {
		return ""
	}
	s = nfc(s)
	return strings.Join(strings.Fields(s), " ")
}

// nfc applies Unicode Normalization Form C. Same canonical form
// across the codebase keeps merge keys stable when callers mix
// pre-composed vs decomposed encodings (e.g. "é" vs "e\u0301").
func nfc(s string) string {
	if s == "" {
		return ""
	}
	return norm.NFC.String(s)
}

// replacePunctuationWithSpace replaces ASCII punctuation runes
// (anything that is unicode.IsPunct or in the ASCII symbol set)
// with a single space, so the predicate column can absorb
// "favorite-color", "favorite/color", "favorite.color" into the
// same token after canonicalSpace.
func replacePunctuationWithSpace(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return b.String()
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
func DefaultMergeKey(f model.TemporalFact) string {
	subject := strings.ToLower(f.Subject)
	predicate := strings.ToLower(f.Predicate)
	object := strings.ToLower(f.Object)

	switch f.Kind {
	case model.KindRelation:
		return joinKey("relation", subject, predicate, object)
	case model.KindState, model.KindPreference:
		if subject != "" && predicate != "" {
			return joinKey(string(f.Kind), subject, predicate)
		}
		return joinKey(string(f.Kind), contentDigest(f))
	case model.KindEvent, model.KindPlan:
		return joinKey(string(f.Kind), subject, predicate, contentDigest(f))
	case model.KindNote:
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

func contentDigest(f model.TemporalFact) string {
	h := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(f.Content))))
	return hex.EncodeToString(h[:8])
}
