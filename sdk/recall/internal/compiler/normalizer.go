package compiler

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/model"
)

// Normalizer canonicalizes free-form fields so deterministic merge
// keys produce stable identities. Phase 1 normalization is intentionally
// shallow; richer schema hardening lands in Phase 4.
type Normalizer interface {
	Normalize(f model.TemporalFact) model.TemporalFact
}

type defaultNormalizer struct{}

func (defaultNormalizer) Normalize(f model.TemporalFact) model.TemporalFact {
	f.Subject = canonicalSpace(f.Subject)
	f.Predicate = canonicalSpace(f.Predicate)
	f.Object = canonicalSpace(f.Object)
	f.Location = canonicalSpace(f.Location)
	f.Content = strings.TrimSpace(f.Content)

	f.Entities = canonicalSet(f.Entities)
	f.Participants = canonicalSet(f.Participants)
	f.SourceMessageIDs = uniqueTrimmed(f.SourceMessageIDs)
	return f
}

func canonicalSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
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
