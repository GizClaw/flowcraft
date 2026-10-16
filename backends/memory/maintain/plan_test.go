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
	} {
		if config.ProducerVersion() == base {
			t.Fatalf("the %s tuning did not change the stamp %q", name, base)
		}
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
