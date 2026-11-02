package maintain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
)

// Relations is the vocabulary of single-valued relations the pass recognizes
// by predicate: a class name (what the relation is -- residence, employer) to
// the predicate phrases a fact may state it with ("lives in", "moved to").
//
// A relation belongs here when one subject can be in it one way at a time: a
// person lives in one place and works for one employer, so a newer fact stating
// the same relation about the same subject makes the older one stale even when
// the two texts share almost no tokens. Relations that hold several values at
// once never belong here, however a model phrases them: facts about
// preferences, events and plans ("attended", "plans to visit"), and
// family-of relations (a parent of several children states one fact each) are
// all current together, so a repeated predicate is not an update.
//
// Membership is a claim about the relation, not about how a model writes it:
// the same relation reached by two phrasings belongs in one class ("moved to"
// and "lives in" both state where someone lives now), and a phrasing that
// states history rather than a current value ("lived in", "was employed by")
// belongs nowhere -- a past state stays true after the next one starts.
//
// Nil means DefaultRelations; an empty but non-nil map turns the rule off,
// which is the honest setting for a deployment whose facts carry no predicates
// at all (the extractor writes them only under its rich strategy).
type Relations map[string][]string

// DefaultRelations returns the built-in vocabulary: the profile relations a
// conversation states about a person that hold one value at a time.
//
// It is deliberately small. The predicate phrases a model writes are its own,
// so a phrase this vocabulary does not recognize leaves the pair to the
// similarity rule -- a missed update, never a wrong one -- and a host that
// knows the phrasings its extractor produces extends the vocabulary (a copy of
// this map plus the phrases) rather than waiting for this list to grow.
func DefaultRelations() Relations {
	return Relations{
		// Where the subject lives now. "moved to" states the residence the
		// move established, which is the relation an older "lives in" states;
		// "moved from" states the place left behind and is not this relation.
		"residence": {
			"lives in", "lives at", "resides in", "resides at",
			"moved to", "moved into", "relocated to", "has moved to",
			"is based in", "based in",
		},
		// Who employs the subject: one employer at a time.
		"employer": {
			"works at", "works for", "employed at", "employed by", "is employed by",
		},
		// What the subject works as: one role at a time, and a different
		// relation from the employer -- "works at Acme" and "works as a
		// bookkeeper" are both current.
		"role": {
			"works as", "employed as",
		},
		// Who the subject is married to or engaged to: one partner at a time.
		"marital status": {
			"married to", "is married to", "engaged to", "is engaged to",
		},
		// Birth facts: one place, one date.
		"birth": {
			"born on", "born in", "was born on", "was born in",
			"birthday is", "date of birth is",
		},
	}
}

// vocabularyEntry is one phrase of one class, in the folded form the pass
// matches and stamps with.
type vocabularyEntry struct {
	class  string
	phrase string
}

// relationIndex is the vocabulary in the shape the pass matches with: one
// folded phrase to the class it states, plus the digest the pass's stamp
// carries. It is built once per pass -- folding a phrase is a comparison key,
// not a lookup, so it is not redone per candidate pair.
type relationIndex struct {
	classes map[string]string
	digest  string
}

// newRelationIndex folds a vocabulary into the pass's shape. The caller has
// validated it (Config.validate), so every entry of canonicalVocabulary is
// usable here; the first entry of a phrase wins, and validation is what makes
// a vocabulary with a phrase in two classes impossible to reach this far.
func newRelationIndex(relations Relations) relationIndex {
	entries, _ := canonicalVocabulary(relations)
	index := relationIndex{classes: make(map[string]string, len(entries))}
	rendered := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, ok := index.classes[entry.phrase]; !ok {
			index.classes[entry.phrase] = entry.class
		}
		rendered = append(rendered, entry.class+"\x00"+entry.phrase)
	}
	sum := sha256.Sum256([]byte(strings.Join(rendered, "\n")))
	index.digest = hex.EncodeToString(sum[:])[:8]
	return index
}

// classOf returns the class a fact's predicate states, and whether the
// vocabulary recognizes it at all. An unset predicate states nothing.
func (index relationIndex) classOf(predicate string) (string, bool) {
	if predicate == "" {
		return "", false
	}
	class, ok := index.classes[normalizePredicate(predicate)]
	return class, ok
}

// Vocabulary is a Relations compiled into the shape the pass matches with: the
// fold and the lookup, built once and asked about many predicates.
//
// It exists so a reader outside this package -- a measurement tool, a host
// diagnostic -- can ask the shipped vocabulary what it thinks of one predicate
// without writing a second copy of the fold. A copy would be the one thing this
// package cannot afford: canonicalVocabulary is where a vocabulary's meaning is
// defined, and a reader that folds differently measures itself rather than the
// pass.
type Vocabulary struct {
	index relationIndex
}

// NewVocabulary compiles relations the way the pass reads them: nil is
// DefaultRelations, and an empty non-nil map recognizes nothing.
func NewVocabulary(relations Relations) Vocabulary {
	if relations == nil {
		relations = DefaultRelations()
	}
	return Vocabulary{index: newRelationIndex(relations)}
}

// ClassOf reports the class a predicate states under this vocabulary, and
// whether the vocabulary recognizes it at all.
func (vocabulary Vocabulary) ClassOf(predicate string) (string, bool) {
	return vocabulary.index.classOf(predicate)
}

// Digest is the vocabulary's identity: the value the pass stamps into
// ProducerVersion, so a reading can say which vocabulary it measured.
func (vocabulary Vocabulary) Digest() string {
	return vocabulary.index.digest
}

// canonicalVocabulary folds a vocabulary into the sorted entries both the
// matching and the stamp are built from, and reports the entries it could not
// use: a class without a name, a phrase that folds to nothing, or a phrase a
// class already claimed (the predicate would name two relations at once, and
// the judgment would depend on which one was read). It is the one place the
// folding is defined, because what a vocabulary is has to mean the same thing
// to the pass that judges with it and to the stamp that identifies it.
func canonicalVocabulary(relations Relations) ([]vocabularyEntry, []string) {
	names := make([]string, 0, len(relations))
	for name := range relations {
		names = append(names, name)
	}
	sort.Strings(names)
	var entries []vocabularyEntry
	var problems []string
	claimed := make(map[string]string, len(relations)*4)
	for _, name := range names {
		// A class name is a label -- the vocabulary identifies the judgments,
		// and the label travels into the finding -- so it folds like the
		// phrases do: two spellings of one class are one class.
		class := normalizePredicate(name)
		if class == "" {
			problems = append(problems, "a relation class has no name")
			continue
		}
		phrases := make([]string, 0, len(relations[name]))
		seen := make(map[string]struct{}, len(relations[name]))
		for _, phrase := range relations[name] {
			normalized := normalizePredicate(phrase)
			if normalized == "" {
				problems = append(problems, fmt.Sprintf("relation %q has a phrase that is empty", class))
				continue
			}
			if existing, ok := claimed[normalized]; ok && existing != class {
				problems = append(problems, fmt.Sprintf("predicate %q states both %q and %q", normalized, existing, class))
				continue
			}
			if _, ok := seen[normalized]; ok {
				// The same phrase twice in one class is a redundant way to
				// write it, not an ambiguous one.
				continue
			}
			claimed[normalized] = class
			seen[normalized] = struct{}{}
			phrases = append(phrases, normalized)
		}
		sort.Strings(phrases)
		for _, phrase := range phrases {
			entries = append(entries, vocabularyEntry{class: class, phrase: phrase})
		}
	}
	return entries, problems
}

// normalizePredicate folds one predicate phrase into the form the vocabulary is
// written in. A predicate is a model's own wording, so the fold is what makes
// the vocabulary a vocabulary: case, separator style, and stray punctuation are
// the model's spelling, not a different relation ("Moved_To", "moved-to", and
// "moved to." are one phrase). Everything else about the text is left alone --
// this maps onto a lookup key, it does not interpret the relation.
func normalizePredicate(value string) string {
	folded := factview.CanonicalContent(value)
	folded = strings.Map(func(character rune) rune {
		switch character {
		case '_', '-', '\u2013', '\u2014':
			return ' '
		}
		return character
	}, folded)
	return strings.Trim(strings.Join(strings.Fields(folded), " "), ".,;:!?")
}
