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
