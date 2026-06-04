package ingest

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func TestNormalizer_UnicodeNFCAndWhitespace(t *testing.T) {
	n := newDefaultNormalizer(nil)
	// "café" in NFD form (e + combining acute) must collapse to
	// the NFC pre-composed form so downstream merge keys are
	// stable regardless of caller encoding.
	got := n.Normalize(domain.TemporalFact{
		Kind:    domain.KindNote,
		Content: "  cafe\u0301  is  nice  ",
		Subject: "Cafe\u0301 Owner",
	})
	if got.Content != "café is nice" {
		t.Errorf("content = %q", got.Content)
	}
	if got.Subject != "Café Owner" {
		t.Errorf("subject = %q", got.Subject)
	}
}

func TestNormalizer_PredicateCollapsesPunctuation(t *testing.T) {
	n := newDefaultNormalizer(nil)
	got := n.Normalize(domain.TemporalFact{
		Kind:      domain.KindState,
		Predicate: "Favourite-Colour",
	})
	if got.Predicate != "favourite colour" {
		t.Errorf("predicate = %q, want %q", got.Predicate, "favourite colour")
	}
}

func TestNormalizer_PredicateSynonymRewrite(t *testing.T) {
	syn := StaticPredicateSynonyms{"favourite colour": "favorite_color"}
	n := newDefaultNormalizer(syn)
	got := n.Normalize(domain.TemporalFact{
		Kind:      domain.KindState,
		Predicate: "Favourite-Colour",
	})
	if got.Predicate != "favorite_color" {
		t.Errorf("predicate after synonym = %q", got.Predicate)
	}
}

func TestNormalizer_IsIdempotent(t *testing.T) {
	n := newDefaultNormalizer(nil)
	fact := domain.TemporalFact{
		Kind:      domain.KindState,
		Subject:   "  Avery  ",
		Predicate: "Favourite.Colour",
		Content:   "  cafe\u0301 ",
	}
	first := n.Normalize(fact)
	second := n.Normalize(first)
	if first.Subject != second.Subject || first.Predicate != second.Predicate || first.Content != second.Content {
		t.Errorf("not idempotent:\nfirst=%+v\nsecond=%+v", first, second)
	}
}

func TestParameterMergeKeyCanonicalizesCondition(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	baseMeta := map[string]any{
		domain.MetaParameterOwner:         "experiment",
		domain.MetaParameterCanonicalName: "temperature",
		domain.MetaParameterValueKind:     "number",
	}
	aMeta := cloneTestMeta(baseMeta)
	aMeta[domain.MetaParameterCondition] = "when GPU is enabled"
	bMeta := cloneTestMeta(baseMeta)
	bMeta[domain.MetaParameterCondition] = "if gpu-is enabled"
	a := domain.TemporalFact{Kind: domain.KindParameter, Scope: scope, Metadata: aMeta}
	b := domain.TemporalFact{Kind: domain.KindParameter, Scope: scope, Metadata: bMeta}
	if gotA, gotB := parameterMergeKey(a), parameterMergeKey(b); gotA != gotB {
		t.Fatalf("parameter merge keys differ:\n%s\n%s", gotA, gotB)
	}
}

func TestParameterMergeKeyIgnoresOperationSurface(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	baseMeta := map[string]any{
		domain.MetaParameterOwner:              "experiment",
		domain.MetaParameterNamespacePath:      "",
		domain.MetaParameterCanonicalName:      "temperature",
		domain.MetaParameterValueKind:          "number",
		domain.MetaParameterNormalizedValue:    "0.2",
		domain.MetaParameterGroundingLevel:     "exact",
		domain.MetaParameterNameSurface:        "temperature",
		domain.MetaParameterNormalizationTrace: "raw",
	}
	setMeta := cloneTestMeta(baseMeta)
	setMeta[domain.MetaParameterOperation] = "set"
	updateMeta := cloneTestMeta(baseMeta)
	updateMeta[domain.MetaParameterOperation] = "update"
	setFact := domain.TemporalFact{Kind: domain.KindParameter, Scope: scope, Metadata: setMeta}
	updateFact := domain.TemporalFact{Kind: domain.KindParameter, Scope: scope, Metadata: updateMeta}
	if gotSet, gotUpdate := parameterMergeKey(setFact), parameterMergeKey(updateFact); gotSet != gotUpdate {
		t.Fatalf("parameter merge keys should match for set/update:\n%s\n%s", gotSet, gotUpdate)
	}
}

func TestNormalizeParameterValueRangeAndEnum(t *testing.T) {
	rng := normalizeParameterValue("0.2..0.8", "")
	if rng.kind != "range" || rng.value != "0.2..0.8" {
		t.Fatalf("range normalized = %+v, want range 0.2..0.8", rng)
	}
	naturalRange := normalizeParameterValue("between 0.2 and 0.8", "")
	if naturalRange.kind == "range" || naturalRange.value == "0.2..0.8" {
		t.Fatalf("natural language range should not be normalized, got %+v", naturalRange)
	}
	naturalBoolean := normalizeParameterValue("enabled", "")
	if naturalBoolean.kind == "boolean" || naturalBoolean.value == "true" {
		t.Fatalf("natural language boolean should not be normalized, got %+v", naturalBoolean)
	}
	enum := normalizeParameterValue("fast_mode", "")
	if enum.kind != "enum" || enum.value != "fast_mode" {
		t.Fatalf("enum normalized = %+v, want enum fast_mode", enum)
	}
}

func cloneTestMeta(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
