package maintain

import (
	"strings"
	"testing"
)

// TestVocabularyClassifiesWithTheShippedFold pins the two things a reader
// outside this package relies on: the fold is the pass's (case, separators and
// stray punctuation are the model's spelling, not a different relation), and the
// digest is the one the pass stamps into ProducerVersion -- a reading that
// reported a different digest would be measuring a different vocabulary.
func TestVocabularyClassifiesWithTheShippedFold(t *testing.T) {
	vocabulary := NewVocabulary(nil)
	for predicate, want := range map[string]string{
		"lives in":       "residence",
		"  LIVES IN.  ":  "residence",
		"Moved_To":       "residence",
		"moved-to":       "residence",
		"is employed by": "employer",
		"works as":       "role",
		"lives":          "",
		"attended":       "",
	} {
		class, ok := vocabulary.ClassOf(predicate)
		if want == "" {
			if ok {
				t.Errorf("ClassOf(%q) = %q, want unrecognized", predicate, class)
			}
			continue
		}
		if !ok || class != want {
			t.Errorf("ClassOf(%q) = %q, %v; want %q", predicate, class, ok, want)
		}
	}

	stamp := Config{}.withDefaults().ProducerVersion()
	if !strings.Contains(stamp, "relations="+vocabulary.Digest()) {
		t.Errorf("ProducerVersion %q does not carry the reading's digest %q",
			stamp, vocabulary.Digest())
	}
}

// TestVocabularyNilReadsAsTheDefault pins the two empty shapes: nil is the
// shipped vocabulary, and an empty but non-nil map is the honest setting for a
// deployment whose facts carry no predicates -- it recognizes nothing, which is
// a different answer from "everything".
func TestVocabularyNilReadsAsTheDefault(t *testing.T) {
	if class, ok := NewVocabulary(nil).ClassOf("lives in"); !ok || class != "residence" {
		t.Errorf("nil vocabulary: ClassOf(lives in) = %q, %v; want residence", class, ok)
	}
	if class, ok := NewVocabulary(Relations{}).ClassOf("lives in"); ok {
		t.Errorf("empty vocabulary: ClassOf(lives in) = %q, want unrecognized", class)
	}

	// The digest identifies the vocabulary, so extending it cannot pass for the
	// vocabulary an existing overlay was judged under.
	extended := NewVocabulary(Relations{"residence": {"lives in", "住在"}})
	if extended.Digest() == NewVocabulary(nil).Digest() {
		t.Error("two vocabularies digest the same")
	}
	if class, ok := extended.ClassOf("住在"); !ok || class != "residence" {
		t.Errorf("ClassOf(住在) = %q, %v; want residence", class, ok)
	}
}
