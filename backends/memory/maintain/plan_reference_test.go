package maintain

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/internal/textsim"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// referenceSupersedes is the judgment the pass made before the entity index:
// for every fact, scan every later fact, and take the first that shares an
// entity and whose text is similar enough. It is written here from the old
// rule rather than shared with the pass, so an index that changes an answer
// has to disagree with this and fail the comparison.
func referenceSupersedes(facts []factview.Fact, threshold float64) []Supersede {
	var findings []Supersede
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
	for index, older := range facts {
		if len(older.Entities) == 0 {
			continue
		}
		for newerIndex := index + 1; newerIndex < len(facts); newerIndex++ {
			newer := facts[newerIndex]
			if len(newer.Entities) == 0 || !shares(older.Entities, newer.Entities) {
				continue
			}
			similarity := textsim.Jaccard(older.Text, newer.Text)
			if similarity < threshold {
				continue
			}
			findings = append(findings, Supersede{
				FactID: older.ID, Conversation: older.ConversationID,
				SupersededBy: newer.ID, Similarity: similarity,
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
// the order the scan visits candidates is the ID tiebreak.
func referenceFixture(seed int64, count int) []factview.Fact {
	random := rand.New(rand.NewSource(seed))
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	vocabulary := []string{"caroline", "lives", "works", "berlin", "acme", "tea", "coffee",
		"shanghai", "since", "moved", "prefers", "morning", "office", "海维", "咖啡"}
	facts := make([]factview.Fact, 0, count)
	for index := 0; index < count; index++ {
		switch index % 11 {
		case 0:
			// A fact with no entities never supersedes and is never superseded.
			facts = append(facts, planFact(fmt.Sprintf("fact-%04d", index),
				"a claim without entities", nil, now.Add(-time.Duration(random.Intn(48))*time.Hour)))
			continue
		case 1:
			facts = append(facts, planFact(fmt.Sprintf("fact-%04d", index),
				"海维喜欢喝美式咖啡", []string{"海维", "海维"}, now.Add(-time.Duration(random.Intn(48))*time.Hour)))
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
		facts = append(facts, planFact(fmt.Sprintf("fact-%04d", index), strings.Join(words, " "), entities, at))
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
		reference := referenceSupersedes(ordered, (Config{}).withDefaults().SimilarityThreshold)
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
	}
}
