package maintain

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/recall"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

func planFact(id, text string, entities []string, eventTime time.Time) factview.Fact {
	return factview.Fact{
		ID: id, Scope: corememory.Scope{RuntimeID: "runtime"}, ConversationID: "conv-1",
		Text: text, Entities: entities, EventTime: eventTime, CreatedAt: eventTime,
	}
}

// planRelationFact is planFact plus the two fields a rich extractor writes and
// the relation rule reads: the predicate the fact states its relation with, and
// the turn's own time expression.
func planRelationFact(
	id, text string,
	entities []string,
	eventTime time.Time,
	predicate, temporalDetail string,
) factview.Fact {
	fact := planFact(id, text, entities, eventTime)
	fact.Predicate, fact.TemporalDetail = predicate, temporalDetail
	return fact
}

func namesEntity(entities []string, name string) bool {
	for _, entity := range entities {
		if strings.EqualFold(strings.TrimSpace(entity), name) {
			return true
		}
	}
	return false
}

// TestProducerVersionNamesTheTuning pins what the overlay's stamp has to carry:
// the policy version alone cannot separate two deployments that tune the same
// policy differently, and what those two write about the same facts differs.
func TestProducerVersionNamesTheTuning(t *testing.T) {
	if (Config{}).ProducerVersion() != (Config{}).withDefaults().ProducerVersion() {
		t.Fatal("the stamp is not stable across defaulting")
	}
	base := (Config{}).ProducerVersion()
	for name, config := range map[string]Config{
		"similarity": {SimilarityThreshold: 0.9},
		"supersede":  {SupersedeFactor: 0.25},
		"half-life":  {DecayHalfLife: 24 * time.Hour},
		"floor":      {DecayFloor: 0.2},
		"saturation": {RecallSaturation: 1},
		"vocabulary": {Relations: Relations{"residence": {"lives in"}}},
	} {
		if config.ProducerVersion() == base {
			t.Fatalf("the %s tuning did not change the stamp %q", name, base)
		}
	}
	// The fold is part of what a vocabulary is, so two spellings of one
	// vocabulary are one pass: the stamp identifies the judgments the pass
	// makes, not the bytes the host wrote.
	spelled := Config{Relations: Relations{"Residence": {"Lives_In", "Moved  To"}}}
	folded := Config{Relations: Relations{"residence": {"lives in", "moved to"}}}
	if spelled.ProducerVersion() != folded.ProducerVersion() {
		t.Fatalf("the same vocabulary spelled two ways has two stamps: %q and %q",
			spelled.ProducerVersion(), folded.ProducerVersion())
	}
	// Every pass carries the stamp it wrote under, so the overlay it saves is
	// read back by the deployment that asked for it and by no other.
	tuned := Config{SimilarityThreshold: 0.9}
	plan, err := DetectWithConfig(
		corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{}, time.Now().UTC(), tuned)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ProducerVersion != tuned.ProducerVersion() {
		t.Fatalf("plan stamp = %q, want %q", plan.ProducerVersion, tuned.ProducerVersion())
	}
	if !strings.Contains(plan.ProducerVersion, AlgorithmVersion) {
		t.Fatalf("plan stamp does not name the policy: %q", plan.ProducerVersion)
	}
}

// TestDetectSupersedesContradictingStableFact pins the contradiction fixture:
// when a newer fact restates the same entity's attribute, the older fact is
// marked superseded by the newer one.
func TestDetectSupersedesContradictingStableFact(t *testing.T) {
	older := planFact("old", "Caroline works at Acme Corporation since 2023.",
		[]string{"Caroline", "Acme Corporation"}, time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))
	newer := planFact("new", "Caroline works at Beta Corporation since 2024.",
		[]string{"Caroline", "Beta Corporation"}, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	plan, err := Detect(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {newer, older},
	}, newer.EventTime.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// The similarity window is the point of the fixture, not decoration:
	// below 0.5 nothing supersedes at all, and the pair shares five of its
	// nine tokens (5/9 = 0.556), so an upper bound catches a similarity
	// that saturates to 1 instead of measuring the overlap.
	if len(plan.Supersedes) != 1 || plan.Supersedes[0].FactID != "old" ||
		plan.Supersedes[0].SupersededBy != "new" ||
		plan.Supersedes[0].Similarity < 0.5 || plan.Supersedes[0].Similarity > 0.7 {
		t.Fatalf("plan = %#v", plan)
	}
	// A different entity must not be superseded.
	unrelated := planFact("other", "Melanie works at Acme Corporation since 2023.",
		[]string{"Melanie", "Acme Corporation"}, time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))
	plan, err = Detect(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {unrelated, newer},
	}, newer.EventTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 0 {
		t.Fatalf("unrelated facts superseded: %#v", plan.Supersedes)
	}
}

// TestDetectSupersedesCJKFactWithoutWordBoundaries pins what the kernel
// tokenizer buys the soft merge: two facts whose only difference sits inside a
// run of CJK characters share most of their tokens once the run is tokenized
// into characters and bigrams, so the pair reads as a contradiction. Split on
// whitespace the two runs are unrelated single tokens and the pair never
// merges.
func TestDetectSupersedesCJKFactWithoutWordBoundaries(t *testing.T) {
	older := planFact("old", "海维喜欢喝美式咖啡", []string{"海维"},
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	newer := planFact("new", "海维喜欢喝拿铁咖啡", []string{"海维"},
		time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	plan, err := Detect(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {newer, older},
	}, newer.EventTime.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// 12 shared tokens out of a 22-token union (0.545): the window is what
	// pins the splitter, since a whitespace splitter scores this pair at 0
	// and no consolidation would happen at all.
	if len(plan.Supersedes) != 1 || plan.Supersedes[0].FactID != "old" ||
		plan.Supersedes[0].SupersededBy != "new" ||
		plan.Supersedes[0].Similarity < 0.5 || plan.Supersedes[0].Similarity > 0.7 {
		t.Fatalf("plan = %#v", plan)
	}
	// Sharing an entity is not on its own a contradiction: a CJK fact about
	// the same subject stays unmerged when its tokens barely overlap.
	unrelated := planFact("other", "海维住在上海", []string{"海维"},
		time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))
	plan, err = Detect(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {newer, unrelated},
	}, newer.EventTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 0 {
		t.Fatalf("unrelated facts superseded: %#v", plan.Supersedes)
	}
}

// TestDetectSupersedesAReplacedValueWithDissimilarText is the O10 acceptance:
// "I moved" updates -- the same single-valued relation restated in different
// words -- are judged by the predicate both facts state, which is the case
// text similarity cannot see. The finding also carries the window the older
// fact was current over, which is what a "was it true then" read needs.
func TestDetectSupersedesAReplacedValueWithDissimilarText(t *testing.T) {
	older := planRelationFact("old", "Caroline lives in Berlin, in the flat above the bakery.",
		[]string{"Caroline", "Berlin"}, time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC),
		"lives in", "since 2019")
	newer := planRelationFact("new",
		"Caroline moved her whole household to Lisbon in the spring of 2024, and the bakery flat is behind her.",
		[]string{"Caroline", "Lisbon"}, time.Date(2024, 5, 2, 0, 0, 0, 0, time.UTC),
		"moved to", "in the spring of 2024")
	plan, err := Detect(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {newer, older},
	}, newer.EventTime.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 1 {
		t.Fatalf("plan = %#v", plan.Supersedes)
	}
	finding := plan.Supersedes[0]
	if finding.FactID != "old" || finding.SupersededBy != "new" ||
		finding.Reason != ReasonRelation || finding.Relation != "residence" {
		t.Fatalf("finding = %+v", finding)
	}
	// The measured similarity is the evidence that the wording alone would not
	// have caught this pair: 5 shared tokens out of a 20-token union (0.25),
	// half the threshold. The window pins that the measure is still made and
	// reported rather than skipped for a finding the text did not decide.
	if finding.Similarity <= 0.1 || finding.Similarity >= 0.5 {
		t.Fatalf("similarity = %v, want the pair's own measure below the threshold", finding.Similarity)
	}
	if !finding.ValidAt.Equal(older.EventTime) || !finding.InvalidAt.Equal(newer.EventTime) {
		t.Fatalf("window = %v..%v, want the two facts' own dates", finding.ValidAt, finding.InvalidAt)
	}
	if finding.ValidDetail != "since 2019" || finding.InvalidDetail != "in the spring of 2024" {
		t.Fatalf("window evidence = %q..%q", finding.ValidDetail, finding.InvalidDetail)
	}
}

// TestDetectLeavesRepeatableRelationsAlone pins what the relation rule must not
// judge. Every case is a candidate pair -- the two facts share the entity named
// in the case -- and every case stays unmerged, because the pair is either
// about a relation that holds several values at once, about two different
// subjects, or about two relations that are both current.
func TestDetectLeavesRepeatableRelationsAlone(t *testing.T) {
	scope := corememory.Scope{RuntimeID: "runtime"}
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	march := planRelationFact("mar", "Caroline attended the pottery class in March 2024.",
		[]string{"Caroline"}, time.Date(2024, 3, 4, 0, 0, 0, 0, time.UTC), "attended", "in March 2024")
	april := planRelationFact("apr", "Caroline attended the choir rehearsal in April 2024.",
		[]string{"Caroline"}, time.Date(2024, 4, 8, 0, 0, 0, 0, time.UTC), "attended", "in April 2024")
	berlin := planRelationFact("ber", "Caroline lives in Berlin.",
		[]string{"Caroline", "Berlin"}, time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC), "lives in", "since 2019")
	melanie := planRelationFact("mel", "Melanie has made Berlin her home.",
		[]string{"Melanie", "Berlin"}, time.Date(2024, 5, 2, 0, 0, 0, 0, time.UTC), "moved to", "in spring 2024")
	moved := planRelationFact("mov", "Caroline moved to Lisbon in January 2024.",
		[]string{"Caroline", "Lisbon"}, time.Date(2024, 1, 9, 0, 0, 0, 0, time.UTC), "moved to", "in January 2024")
	lives := planRelationFact("liv", "Caroline lives in Lisbon.",
		[]string{"Caroline", "Lisbon"}, time.Date(2024, 1, 20, 0, 0, 0, 0, time.UTC), "lives in", "since January 2024")
	observatory := planRelationFact("obs", "Caroline works at the Berlin Observatory, which she joined in 2024.",
		[]string{"Caroline", "Berlin", "Berlin Observatory"}, time.Date(2024, 2, 2, 0, 0, 0, 0, time.UTC), "works at", "since 2024")
	cases := []struct {
		name           string
		older, newer   factview.Fact
		shared, reason string
	}{
		{
			name: "an event the subject can repeat", older: march, newer: april, shared: "Caroline",
			reason: "two classes in two months are both current; \"attended\" holds several values",
		},
		{
			name: "the same relation about two subjects", older: berlin, newer: melanie, shared: "Berlin",
			reason: "Caroline's residence does not go stale because Melanie has one in the same city",
		},
		{
			name: "the same value stated twice", older: moved, newer: lives, shared: "Caroline",
			reason: "moving to Lisbon and living in Lisbon agree about the place, so the older fact is compatible",
		},
		{
			name: "two relations that are both current", older: berlin, newer: observatory, shared: "Berlin",
			reason: "where Caroline lives and who she works for are different relations",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if !namesEntity(test.older.Entities, test.shared) || !namesEntity(test.newer.Entities, test.shared) {
				t.Fatalf("the fixture no longer shares %q, so it is not a candidate pair", test.shared)
			}
			plan, err := Detect(scope, map[string][]factview.Fact{"conv-1": {test.newer, test.older}}, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Supersedes) != 0 {
				t.Fatalf("superseded: %+v (want none: %s)", plan.Supersedes, test.reason)
			}
		})
	}
}

// TestDetectReadsAModelsSpellingOfAPredicate pins the fold. A predicate is the
// model's own wording, so case, separators, and a trailing period are the same
// predicate -- the vocabulary is matched, not the bytes.
func TestDetectReadsAModelsSpellingOfAPredicate(t *testing.T) {
	older := planRelationFact("old", "Caroline lives in Berlin, above the bakery.",
		[]string{"Caroline", "Berlin"}, time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC),
		"Lives_In", "since 2019")
	newer := planRelationFact("new", "Caroline traded Berlin for Lisbon last spring.",
		[]string{"Caroline", "Lisbon"}, time.Date(2024, 5, 2, 0, 0, 0, 0, time.UTC),
		"Moved-To.", "last spring")
	plan, err := Detect(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {newer, older},
	}, newer.EventTime.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 1 || plan.Supersedes[0].Reason != ReasonRelation ||
		plan.Supersedes[0].Relation != "residence" {
		t.Fatalf("plan = %#v", plan.Supersedes)
	}
}

// TestDetectTurnsTheRelationRuleOff pins the vocabulary as what makes the
// judgment: the acceptance fixture judged with an empty vocabulary is not
// judged at all, because the texts alone do not reach the similarity
// threshold. A deployment whose facts carry no predicates configures that map.
func TestDetectTurnsTheRelationRuleOff(t *testing.T) {
	older := planRelationFact("old", "Caroline lives in Berlin, in the flat above the bakery.",
		[]string{"Caroline", "Berlin"}, time.Date(2019, 6, 1, 0, 0, 0, 0, time.UTC),
		"lives in", "since 2019")
	newer := planRelationFact("new",
		"Caroline moved her whole household to Lisbon in the spring of 2024, and the bakery flat is behind her.",
		[]string{"Caroline", "Lisbon"}, time.Date(2024, 5, 2, 0, 0, 0, 0, time.UTC),
		"moved to", "in the spring of 2024")
	now := newer.EventTime.Add(24 * time.Hour)
	plan, err := DetectWithConfig(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {newer, older},
	}, now, Config{Relations: Relations{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 0 {
		t.Fatalf("an empty vocabulary still judged: %#v", plan.Supersedes)
	}
	plan, err = Detect(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {newer, older},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 1 {
		t.Fatalf("the default vocabulary judged nothing: %#v", plan.Supersedes)
	}
}

// TestDetectDatesTheWindowTheSupersededFactWasCurrentOver pins what the window
// is measured from: the superseded fact's own date -- its event time, or when
// the fact was created if the turn stated no time -- to the date of the fact
// that replaced it, which is the key the pass ordered the pair by. A window
// never runs backwards, and a turn that stated no time leaves its evidence
// empty rather than guessed.
func TestDetectDatesTheWindowTheSupersededFactWasCurrentOver(t *testing.T) {
	undated := planRelationFact("undated", "Caroline lives in Berlin, and the flat is in Prenzlauer Berg.",
		[]string{"Caroline", "Berlin"}, time.Time{}, "lives in", "")
	undated.CreatedAt = time.Date(2023, 5, 5, 0, 0, 0, 0, time.UTC)
	newer := planRelationFact("moved", "Caroline left Berlin and set up in Lisbon.",
		[]string{"Caroline", "Lisbon"}, time.Date(2024, 5, 2, 0, 0, 0, 0, time.UTC),
		"moved to", "in the spring of 2024")
	plan, err := Detect(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {newer, undated},
	}, newer.EventTime.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 1 {
		t.Fatalf("plan = %#v", plan.Supersedes)
	}
	finding := plan.Supersedes[0]
	if !finding.ValidAt.Equal(undated.CreatedAt) || !finding.ValidAt.Before(finding.InvalidAt) {
		t.Fatalf("window = %v..%v, want the created-at fallback", finding.ValidAt, finding.InvalidAt)
	}
	if !finding.InvalidAt.Equal(newer.EventTime) || finding.ValidDetail != "" ||
		finding.InvalidDetail != "in the spring of 2024" {
		t.Fatalf("finding = %+v", finding)
	}
}

// TestDetectDecaysAgedFacts pins the decay half-life.
func TestDetectDecaysAgedFacts(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	aged := planFact("aged", "Caroline works at Acme Corporation.", []string{"Caroline"}, now.Add(-800*time.Hour))
	fresh := planFact("fresh", "Caroline lives in Shanghai.", []string{"Caroline"}, now.Add(-24*time.Hour))
	plan, err := Detect(corememory.Scope{RuntimeID: "runtime"}, map[string][]factview.Fact{
		"conv-1": {aged, fresh},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Decays) != 1 || plan.Decays[0].FactID != "aged" || plan.Decays[0].Score >= 0.5 {
		t.Fatalf("plan = %#v", plan)
	}
	for _, decay := range plan.Decays {
		if decay.FactID == "fresh" {
			t.Fatalf("fresh fact decayed: %#v", plan.Decays)
		}
	}
}

func TestDetectValidatesConfigAndScope(t *testing.T) {
	if _, err := Detect(corememory.Scope{}, nil, time.Now()); err == nil {
		t.Fatal("invalid scope accepted")
	}
	_, err := DetectWithConfig(corememory.Scope{RuntimeID: "runtime"}, nil, time.Now(),
		Config{SimilarityThreshold: 2})
	if err == nil {
		t.Fatal("invalid similarity threshold accepted")
	}
	_, err = DetectWithConfig(corememory.Scope{RuntimeID: "runtime"}, nil, time.Now(),
		Config{RecallSaturation: -1})
	if err == nil {
		t.Fatal("negative recall saturation accepted")
	}
	for name, relations := range map[string]Relations{
		"unnamed class": {"": {"lives in"}},
		"empty phrase":  {"residence": {"  "}},
		"two classes":   {"residence": {"lives in"}, "employer": {"Lives_In"}},
	} {
		_, err := DetectWithConfig(corememory.Scope{RuntimeID: "runtime"}, nil, time.Now(),
			Config{Relations: relations})
		if err == nil {
			t.Fatalf("a vocabulary with a %s accepted", name)
		}
	}
	// The same phrase twice in one class is a redundant way to write it, not
	// an ambiguous one.
	if _, err := DetectWithConfig(corememory.Scope{RuntimeID: "runtime"}, nil, time.Now(),
		Config{Relations: Relations{"residence": {"lives in", "Lives_In"}}}); err != nil {
		t.Fatalf("repeated phrase refused: %v", err)
	}
}

// TestDetectCompensatesRecalledFacts pins the v3 reinforcement factor. The
// decay clock reads from max(event time, last recall), and each receipt
// forgives count/(count+RecallSaturation) of the decay that remains -- so a
// fact's compensated score stays in [decay, 1] and a recall can rescue an
// aged fact from the overlay without ever outranking a fresh one. A fact the
// read path never returned keeps exactly the v2 score, which is what makes
// an import decay like an import.
func TestDetectCompensatesRecalledFacts(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	scope := corememory.Scope{RuntimeID: "runtime"}
	eventTime := now.Add(-1250 * time.Hour)
	identity := func(factID string) string {
		t.Helper()
		key, err := itemKey(scope, "conv-1", factID)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	facts := map[string][]factview.Fact{
		"conv-1": {
			planFact("aged", "Alice works at Acme.", []string{"Alice"}, eventTime),
			planFact("rehearsed", "Bob works at Beta.", []string{"Bob"}, eventTime),
			planFact("recent", "Carol works at Gamma.", []string{"Carol"}, eventTime),
			planFact("stale-recall", "Dan works at Delta.", []string{"Dan"}, eventTime),
		},
	}
	rehearsedAt := eventTime
	receipts := map[string]recall.Receipt{
		identity("rehearsed"):    {Identity: identity("rehearsed"), Count: 1, Last: rehearsedAt, LastEvent: "run-1"},
		identity("recent"):       {Identity: identity("recent"), Count: 8, Last: now.Add(-time.Hour), LastEvent: "run-2"},
		identity("stale-recall"): {Identity: identity("stale-recall"), Count: 8, Last: now.Add(-2500 * time.Hour), LastEvent: "run-3"},
	}

	plan, err := DetectWithRecalls(scope, facts, now, Config{}, receipts)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RecalledFacts != 3 {
		t.Fatalf("plan recalls %d facts, want the 3 with receipts: %#v", plan.RecalledFacts, plan)
	}
	scores := make(map[string]Decay, len(plan.Decays))
	for _, decay := range plan.Decays {
		scores[decay.FactID] = decay
	}
	raw := scores["aged"]
	// 1250h of a 720h half-life: 2^-(1250/720) = 0.3001.
	if math.Abs(raw.Score-0.3001) > 5e-4 || raw.Recalls != 0 || !raw.LastRecall.IsZero() {
		t.Fatalf("unrecalled decay = %#v", raw)
	}
	// One receipt forgives 1/9 of the gap: 0.3001 + 0.6999/9 = 0.3779. The
	// recall is older than the event, so the basis -- and with it the raw
	// decay -- does not move; only the compensation does.
	compensated := scores["rehearsed"]
	if math.Abs(compensated.Score-0.3779) > 5e-4 || compensated.Recalls != 1 ||
		!compensated.LastRecall.Equal(rehearsedAt) || !compensated.EventTime.Equal(eventTime) {
		t.Fatalf("compensated decay = %#v", compensated)
	}
	// A recall inside the half-life moves the basis to the recall itself, so
	// the fact stops being due for decay at all; and a fact recalled enough
	// times to climb back above the floor is not written either -- the floor
	// is where decay starts to matter, and reinforcement decides whether it
	// does.
	for _, id := range []string{"recent", "stale-recall"} {
		if entry, ok := scores[id]; ok {
			t.Fatalf("recalled fact %q still decayed: %#v", id, entry)
		}
	}

	// Without receipts the same input is the v2 judgment under the v3 stamp:
	// every aged fact decays from its event time.
	plain, err := DetectWithRecalls(scope, facts, now, Config{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain.RecalledFacts != 0 || len(plain.Decays) != 4 {
		t.Fatalf("plan without receipts = %#v", plain)
	}
	for _, decay := range plain.Decays {
		if decay.Recalls != 0 || !decay.LastRecall.IsZero() {
			t.Fatalf("plan without receipts names one: %#v", decay)
		}
	}
}
