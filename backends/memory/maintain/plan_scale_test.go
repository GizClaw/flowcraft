package maintain

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

// scaleVocabulary is the pool the filler words of the scale fixture are drawn
// from. The words are distinct enough that two facts built from different
// draws share almost no tokens, which is the point: the fixture's majority is
// facts that share an entity but not a claim.
var scaleVocabulary = strings.Fields(`
amber anchor basil beacon billet bramble cinder clover cobble compass copper crag
dawn delta drift ember fable fathom fern flint fodder gable garland glade gorge
granite harbor heather hollow inlet ironwood juniper kettle lagoon lantern leeward
linden marble meadow mesa mistral moss myrtle nimbus northward oakmoss orchard
pebble pines plank quarry quartz quiet ravine reed ridge rivulet rowan saddle
sage sandbar shale sierra slate soar sorrel spruce stonecrop thicket thistle
timber tundra valley verdant vine wattle willow windward yarrow zephyr`)

// scaleText builds one fact's text: the fact's own number, its entities, and
// twelve filler words chosen by the index. Two facts of the same entity share
// the entity tokens and almost nothing else -- about 2 of 16 tokens -- so a
// scan has to keep looking instead of matching and breaking out.
func scaleText(index int, entities []string) string {
	words := make([]string, 0, len(scaleVocabulary)/4)
	words = append(words, fmt.Sprintf("note-%d", index))
	words = append(words, entities...)
	state := uint64(index)*2654435761 + 12345
	for len(words) < 14 {
		state = state*6364136223846793005 + 1442695040888963407
		words = append(words, scaleVocabulary[(state>>33)%uint64(len(scaleVocabulary))])
	}
	return strings.Join(words, " ")
}

// scaleFacts builds the O5 fixture scope: ten thousand facts of one
// conversation. Nine thousand of them carry two entities out of a pool of
// five hundred and next to no shared wording -- the bulk a scan has to skip --
// and one thousand are the "Caroline" family, which shares one entity and
// near-identical text so the supersede path does real similarity work and
// reports real findings. The family also states a predicate the default
// vocabulary recognizes, so the relation rule is asked about the family's pairs
// on a scope this size. The scan still stops at the first judgment of a fact,
// so what this measures is the rule's cost per judged fact, not the cost of
// scanning every candidate of one: a fixture of facts that state the same
// relation about one subject in *dissimilar* words is what would do that, and
// it is the reference fixture's job.
func scaleFacts(now time.Time) []factview.Fact {
	facts := make([]factview.Fact, 0, 10_000)
	for index := 0; index < 10_000; index++ {
		entity := fmt.Sprintf("entity-%d", index%500)
		other := fmt.Sprintf("entity-%d", (index*7+13)%500)
		entities := []string{entity, other}
		text := scaleText(index, entities)
		predicate := ""
		if index%10 == 0 {
			entities = append(entities, "Caroline")
			text = fmt.Sprintf("Caroline works at Acme Corporation since %d, in the Berlin office", 2000+index%20)
			predicate = "works at"
		}
		fact := planFact(fmt.Sprintf("fact-%d", index), text, entities,
			now.Add(-time.Duration(index%1440)*time.Hour))
		fact.Predicate = predicate
		facts = append(facts, fact)
	}
	return facts
}

// TestDetectScale pins what a maintenance pass costs on the fixture scope.
// The assertion is the finding, not the wall clock: a pruned scan and a
// pair-wise scan must agree, and only the pruned one finishes a scope this
// size in a test run. The timing is logged for the record, together with what
// publishing the pass costs on a real store.
func TestDetectScale(t *testing.T) {
	if testing.Short() {
		t.Skip("scale fixture")
	}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	scope := corememory.Scope{RuntimeID: "runtime"}
	facts := scaleFacts(now)

	started := time.Now()
	plan, err := Detect(scope, map[string][]factview.Fact{"conv-1": facts}, now)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	// A fact older than one half-life (the decay floor is 0.5) is decayed and
	// a younger one is not: the pass owes one entry per aged fact.
	expected := 0
	for _, fact := range facts {
		if now.Sub(fact.EventTime) > 720*time.Hour {
			expected++
		}
	}
	t.Logf("detect: %v for %d facts, %d decays (want %d), %d supersedes",
		elapsed, len(facts), len(plan.Decays), expected, len(plan.Supersedes))
	if len(plan.Decays) != expected {
		t.Fatalf("decays = %d, want %d", len(plan.Decays), expected)
	}
	if len(plan.Supersedes) < 500 {
		t.Fatalf("supersedes = %d, want the fixture's family judged", len(plan.Supersedes))
	}
	store, _ := testStore(t)
	saveStarted := time.Now()
	if err := store.Save(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	t.Logf("store: %v to publish %d entries", time.Since(saveStarted), len(plan.Decays)+len(plan.Supersedes))
}
