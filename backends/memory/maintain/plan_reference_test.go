package maintain

import (
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/internal/textsim"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// referenceSupersedes is the judgment the pass makes, written as the pair-wise
// scan the entity index replaced: for every fact, scan every later fact, and
// take the first that shares an entity and is either the same single-valued
// relation about the same subject with a different value, or similar enough by
// text. It is written here from the rule rather than shared with the pass, so
// an index -- or a subject read, or a value comparison -- that changes an
// answer has to disagree with this and fail the comparison.
func referenceSupersedes(facts []factview.Fact, threshold float64, vocabulary Relations) []Supersede {
	var findings []Supersede
	classes := newRelationIndex(vocabulary)
	shares := func(left, right []string) bool {
		for _, a := range left {
			for _, b := range right {
				// The pass folds names with TrimSpace and ToLower; the
				// fixture's names are ASCII and CJK, where EqualFold over
				// the trimmed names agrees with that fold.
				if strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b)) {
					return true
				}
			}
		}
		return false
	}
	names := func(entities []string) []string {
		normalized := make([]string, 0, len(entities))
		for _, entity := range entities {
			entity = strings.ToLower(strings.TrimSpace(entity))
			if entity == "" {
				continue
			}
			normalized = append(normalized, entity)
		}
		sort.Strings(normalized)
		return slices.Compact(normalized)
	}
	// The subject is the entity the fact's own text names first, read the way
	// the pass reads it, and the objects are the entities it names besides.
	subject := func(fact factview.Fact) string {
		text := factview.CanonicalContent(fact.Text)
		found, at := "", -1
		for _, entity := range names(fact.Entities) {
			offset := strings.Index(text, entity)
			if offset < 0 {
				continue
			}
			if at < 0 || offset < at || (offset == at && entity < found) {
				found, at = entity, offset
			}
		}
		return found
	}
	objects := func(fact factview.Fact, about string) []string {
		kept := make([]string, 0, len(fact.Entities))
		for _, entity := range names(fact.Entities) {
			if entity != about {
				kept = append(kept, entity)
			}
		}
		return kept
	}
	differentValue := func(older, newer factview.Fact, about string) bool {
		olderObjects, newerObjects := objects(older, about), objects(newer, about)
		if len(olderObjects) == 0 || len(newerObjects) == 0 {
			return true
		}
		return !slices.Equal(olderObjects, newerObjects)
	}
	for index, older := range facts {
		if len(older.Entities) == 0 {
			continue
		}
		olderClass, olderStates := classes.classOf(older.Predicate)
		olderSubject := ""
		if olderStates {
			olderSubject = subject(older)
		}
		for newerIndex := index + 1; newerIndex < len(facts); newerIndex++ {
			newer := facts[newerIndex]
			if len(newer.Entities) == 0 || !shares(older.Entities, newer.Entities) {
				continue
			}
			similarity := textsim.Jaccard(older.Text, newer.Text)
			reason, relation := "", ""
			if olderStates && olderSubject != "" {
				if class, ok := classes.classOf(newer.Predicate); ok && class == olderClass &&
					subject(newer) == olderSubject && differentValue(older, newer, olderSubject) {
					reason, relation = ReasonRelation, class
				}
			}
			if reason == "" && similarity < threshold {
				continue
			}
			if reason == "" {
				reason = ReasonSimilarity
			}
			findings = append(findings, Supersede{
				FactID: older.ID, Conversation: older.ConversationID,
				SupersededBy: newer.ID, Similarity: similarity,
				Reason: reason, Relation: relation,
				ValidAt: recencyKey(older), InvalidAt: recencyKey(newer),
				ValidDetail: older.TemporalDetail, InvalidDetail: newer.TemporalDetail,
			})
			break
		}
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].FactID < findings[j].FactID })
	return findings
}

// referenceFixture builds a conversation whose edges the reference scan is
// cheap enough to judge whole: entity pools small enough that facts share
// entities constantly, texts drawn from a small vocabulary so similarity
// lands on both sides of the threshold, duplicated and empty entity names,
// facts without entities, CJK text, and event-time ties -- the last because
// the order the scan visits candidates is the ID tiebreak. Every fact also
// states a predicate out of a pool that mixes phrases the default vocabulary
// recognizes, phrases spelled the way a model spells them, and phrases it does
// not recognize at all, so the relation half of the rule is judged here too.
func referenceFixture(seed int64, count int) []factview.Fact {
	random := rand.New(rand.NewSource(seed))
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	vocabulary := []string{"caroline", "lives", "works", "berlin", "acme", "tea", "coffee",
		"shanghai", "since", "moved", "prefers", "morning", "office", "海维", "咖啡"}
	predicates := []string{"", "lives in", "Lives_In", "moved to", "works at", "works as",
		"attended", "plans to visit"}
	details := []string{"", "since 2019", "on 5 March 2023", "last summer"}
	facts := make([]factview.Fact, 0, count)
	for index := 0; index < count; index++ {
		switch index % 11 {
		case 0:
			// A fact with no entities never supersedes and is never superseded.
			unmarked := planFact(fmt.Sprintf("fact-%04d", index),
				"a claim without entities", nil, now.Add(-time.Duration(random.Intn(48))*time.Hour))
			unmarked.Predicate = predicates[index%len(predicates)]
			unmarked.TemporalDetail = details[index%len(details)]
			facts = append(facts, unmarked)
			continue
		case 1:
			repeated := planFact(fmt.Sprintf("fact-%04d", index),
				"海维喜欢喝美式咖啡", []string{"海维", "海维"}, now.Add(-time.Duration(random.Intn(48))*time.Hour))
			repeated.Predicate = predicates[index%len(predicates)]
			repeated.TemporalDetail = details[index%len(details)]
			facts = append(facts, repeated)
			continue
		}
		entity := fmt.Sprintf("entity-%d", random.Intn(12))
		other := fmt.Sprintf("entity-%d", random.Intn(12))
		entities := []string{entity, other}
		if index%7 == 0 {
			entities = append(entities, entities[0], "") // duplicates and an empty name
		}
		words := make([]string, 0, 6)
		for len(words) < 6 {
			words = append(words, vocabulary[random.Intn(len(vocabulary))])
		}
		// Ties are deliberate: the scan breaks them by ID.
		at := now.Add(-time.Duration(random.Intn(4)) * time.Hour)
		fact := planFact(fmt.Sprintf("fact-%04d", index), strings.Join(words, " "), entities, at)
		fact.Predicate = predicates[index%len(predicates)]
		fact.TemporalDetail = details[index%len(details)]
		facts = append(facts, fact)
	}
	return facts
}

// TestDetectAgreesWithThePairwiseReference is the index's acceptance: on
// fixtures dense with shared entities, duplicate names, empty names, CJK
// text, and recency ties, the pruned scan judges exactly what the pair-wise
// scan judged -- same findings, same order, same similarity.
func TestDetectAgreesWithThePairwiseReference(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	scope := corememory.Scope{RuntimeID: "runtime"}
	for _, seed := range []int64{1, 2, 3, 4, 5} {
		facts := referenceFixture(seed, 600)
		ordered := append([]factview.Fact(nil), facts...)
		sort.SliceStable(ordered, func(i, j int) bool {
			left, right := recencyKey(ordered[i]), recencyKey(ordered[j])
			if left.Equal(right) {
				return ordered[i].ID < ordered[j].ID
			}
			return left.Before(right)
		})

		plan, err := Detect(scope, map[string][]factview.Fact{"conv-1": ordered}, now)
		if err != nil {
			t.Fatal(err)
		}
		reference := referenceSupersedes(ordered, (Config{}).withDefaults().SimilarityThreshold, DefaultRelations())
		if len(plan.Supersedes) != len(reference) {
			t.Fatalf("seed %d: %d findings, reference found %d", seed, len(plan.Supersedes), len(reference))
		}
		for index := range reference {
			if plan.Supersedes[index] != reference[index] {
				t.Fatalf("seed %d: finding %d = %+v, reference %+v", seed, index, plan.Supersedes[index], reference[index])
			}
		}
		if len(reference) == 0 {
			t.Fatalf("seed %d: the fixture found nothing; it is not exercising the scan", seed)
		}
		// Both halves of the rule have to be exercised, or the comparison is
		// agreeing about a fixture that never asks the new question.
		reasons := map[string]int{}
		for _, finding := range reference {
			reasons[finding.Reason]++
		}
		if reasons[ReasonRelation] == 0 || reasons[ReasonSimilarity] == 0 {
			t.Fatalf("seed %d: reference reasons = %#v, want both halves judged", seed, reasons)
		}
	}
}
