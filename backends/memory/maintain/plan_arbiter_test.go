package maintain

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	factview "github.com/GizClaw/flowcraft/backends/memory/views/fact"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// stubArbiter answers from a function and records what it was asked, which is
// the two things a pass-level test needs: the pairs the rules left undecided,
// and a judgment the test writes by hand.
type stubArbiter struct {
	identity string
	answer   func(Pair) Judgment
	err      error
	asked    []Pair
}

func (stub *stubArbiter) Identity() string {
	if stub.identity == "" {
		return "stub/arbiter@test,p=0.75"
	}
	return stub.identity
}

func (stub *stubArbiter) Adjudicate(_ context.Context, pairs []Pair) ([]Judgment, error) {
	stub.asked = append(stub.asked, pairs...)
	if stub.err != nil {
		return nil, stub.err
	}
	judgments := make([]Judgment, len(pairs))
	for index, pair := range pairs {
		if stub.answer == nil {
			judgments[index] = Judgment{Verdict: VerdictBothCurrent}
			continue
		}
		judgments[index] = stub.answer(pair)
	}
	return judgments, nil
}

// alwaysReplaced is the arbiter that calls every pair an update, which is what
// a test uses to prove the pass asked at all.
func alwaysReplaced(Pair) Judgment {
	return Judgment{Verdict: VerdictReplaced, Confidence: 0.93}
}

// undecidedFacts builds the scope the arbiter tests judge: one conversation of
// facts ordered by their event times.
func undecidedFacts(texts ...factview.Fact) map[string][]factview.Fact {
	return map[string][]factview.Fact{"conv-1": texts}
}

func fact(id, text string, entities []string, at time.Time) factview.Fact {
	return factview.Fact{
		ID: id, Text: text, Entities: entities, ConversationID: "conv-1",
		EventTime: at,
	}
}

func detectWith(t *testing.T, config Config, conversations map[string][]factview.Fact) (Plan, *stubArbiter) {
	t.Helper()
	stub := &stubArbiter{answer: alwaysReplaced}
	config.Arbiter = stub
	plan, pairs, err := detect(
		corememory.Scope{RuntimeID: "runtime", UserID: "user"},
		conversations, time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := adjudicate(context.Background(), &plan, pairs, stub); err != nil {
		t.Fatal(err)
	}
	return plan, stub
}

// TestDetectAsksTheArbiterWhatTheRulesCannotDecide is the case the arbiter
// exists for: a Chinese update whose predicates the vocabulary has no name for
// and whose texts share almost no tokens. Neither rule sees it; the arbiter is
// asked, and its verdict becomes a finding that records the reason and the
// probability it was judged with.
func TestDetectAsksTheArbiterWhatTheRulesCannotDecide(t *testing.T) {
	older := time.Date(2021, 5, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2023, 8, 14, 0, 0, 0, 0, time.UTC)
	plan, stub := detectWith(t, Config{}, undecidedFacts(
		fact("fact-1", "郑凯住在柏林。", []string{"郑凯", "柏林"}, older),
		fact("fact-2", "郑凯上个月搬到了上海。", []string{"郑凯"}, newer),
	))
	if len(stub.asked) != 1 {
		t.Fatalf("arbiter was asked %d pairs: %#v", len(stub.asked), stub.asked)
	}
	if stub.asked[0].Older.ID != "fact-1" || stub.asked[0].Newer.ID != "fact-2" {
		t.Fatalf("pair = %s -> %s", stub.asked[0].Older.ID, stub.asked[0].Newer.ID)
	}
	if len(plan.Supersedes) != 1 {
		t.Fatalf("supersedes = %#v", plan.Supersedes)
	}
	finding := plan.Supersedes[0]
	if finding.FactID != "fact-1" || finding.SupersededBy != "fact-2" {
		t.Fatalf("finding = %#v", finding)
	}
	if finding.Reason != ReasonArbiter {
		t.Errorf("reason = %q, want %q", finding.Reason, ReasonArbiter)
	}
	if finding.Confidence != 0.93 {
		t.Errorf("confidence = %v", finding.Confidence)
	}
	if !finding.ValidAt.Equal(older) || !finding.InvalidAt.Equal(newer) {
		t.Errorf("window = %s..%s", finding.ValidAt, finding.InvalidAt)
	}
	if plan.ArbiterAsked != 1 || plan.ArbiterSkipped != 0 || plan.ArbiterUndecided != 0 {
		t.Errorf("counts = asked %d, skipped %d, undecided %d",
			plan.ArbiterAsked, plan.ArbiterSkipped, plan.ArbiterUndecided)
	}
}

// TestDetectDoesNotAskAboutPairsTheVocabularyDecides pins the precedence: the
// rules run first and a rule's finding is the pass's answer. The arbiter is
// the third rule, not a second opinion on the first two, and a pass that asked
// about a pair it already decided would pay for an answer it cannot use.
func TestDetectDoesNotAskAboutPairsTheVocabularyDecides(t *testing.T) {
	older := fact("fact-1", "Caroline lives in Berlin.", []string{"Caroline", "Berlin"}, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
	older.Predicate = "lives in"
	newer := fact("fact-2", "Caroline lives in Paris.", []string{"Caroline", "Paris"}, time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC))
	newer.Predicate = "lives in"
	plan, stub := detectWith(t, Config{}, undecidedFacts(older, newer))
	if len(stub.asked) != 0 {
		t.Fatalf("arbiter was asked %#v", stub.asked)
	}
	if len(plan.Supersedes) != 1 || plan.Supersedes[0].Reason != ReasonRelation {
		t.Fatalf("supersedes = %#v", plan.Supersedes)
	}
}

// TestDetectProposesOnlyPairsAboutOneSubject pins the candidate rule: the
// facts have to be about the same subject for the pair to be one relation
// stated twice. Two facts that merely share an entity are two facts that
// mention the same name, and asking a model about every such pair is how a
// pass spends a budget saying "both-current" to itself.
func TestDetectProposesOnlyPairsAboutOneSubject(t *testing.T) {
	_, stub := detectWith(t, Config{}, undecidedFacts(
		// Berlin is the subject here; Caroline is only mentioned.
		fact("fact-1", "Berlin is where Caroline wants to live.", []string{"Berlin", "Caroline"}, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)),
		fact("fact-2", "Caroline is thinking about Lisbon.", []string{"Caroline", "Lisbon"}, time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)),
	))
	if len(stub.asked) != 0 {
		t.Fatalf("arbiter was asked %#v", stub.asked)
	}
}

// TestDetectLeavesEntityFreeFactsUnproposed records the bound the pass has: a
// fact the extractor wrote without a named entity has no subject to draw
// candidates by, so neither the rules nor the arbiter sees it. The reading is
// what says how much of a corpus that is; the test is what stops the bound
// from being forgotten.
func TestDetectLeavesEntityFreeFactsUnproposed(t *testing.T) {
	plan, stub := detectWith(t, Config{}, undecidedFacts(
		fact("fact-1", "User lives in Berlin.", nil, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)),
		fact("fact-2", "User moved to Paris.", nil, time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)),
	))
	if len(stub.asked) != 0 || len(plan.Supersedes) != 0 {
		t.Fatalf("asked %#v, supersedes %#v", stub.asked, plan.Supersedes)
	}
}

// TestDetectSpendsTheArbiterBudgetInScanOrder pins the cost bound: a pass asks
// at most what the budget allows, spends it in the order the scan proposed the
// pairs, and reports what it left for a later pass instead of hiding it.
func TestDetectSpendsTheArbiterBudgetInScanOrder(t *testing.T) {
	at := func(month int) time.Time { return time.Date(2021, time.Month(month), 1, 0, 0, 0, 0, time.UTC) }
	plan, stub := detectWith(t, Config{ArbiterBudget: 2}, undecidedFacts(
		fact("fact-1", "郑凯住在柏林。", []string{"郑凯", "柏林"}, at(1)),
		fact("fact-2", "郑凯搬去了巴黎。", []string{"郑凯", "巴黎"}, at(2)),
		fact("fact-3", "郑凯又搬去了罗马。", []string{"郑凯", "罗马"}, at(3)),
		fact("fact-4", "郑凯最后搬去了东京。", []string{"郑凯", "东京"}, at(4)),
	))
	if len(stub.asked) != 2 {
		t.Fatalf("asked %d pairs: %#v", len(stub.asked), stub.asked)
	}
	if plan.ArbiterAsked != 2 || plan.ArbiterSkipped != 4 {
		t.Fatalf("counts = asked %d, skipped %d", plan.ArbiterAsked, plan.ArbiterSkipped)
	}
	if stub.asked[0].Older.ID != "fact-1" || stub.asked[1].Older.ID != "fact-1" {
		t.Fatalf("the budget did not follow the scan order: %#v", stub.asked)
	}
}

// TestDetectRecordsOneFindingPerFact pins the finding rule: a fact has one
// current value, so several later facts cannot each replace it. The earliest
// later fact that made it stale is the one recorded, which is the same choice
// the rules make when they stop at the first deciding candidate.
func TestDetectRecordsOneFindingPerFact(t *testing.T) {
	at := func(month int) time.Time { return time.Date(2021, time.Month(month), 1, 0, 0, 0, 0, time.UTC) }
	plan, stub := detectWith(t, Config{}, undecidedFacts(
		fact("fact-1", "郑凯住在柏林。", []string{"郑凯", "柏林"}, at(1)),
		fact("fact-2", "郑凯搬去了巴黎。", []string{"郑凯", "巴黎"}, at(2)),
		fact("fact-3", "郑凯又搬去了罗马。", []string{"郑凯", "罗马"}, at(3)),
	))
	// Three facts, three candidate pairs, and the arbiter calls all of them
	// updates because the stub always does.
	if len(stub.asked) != 3 {
		t.Fatalf("asked %d pairs", len(stub.asked))
	}
	// Each fact still gets at most one finding: fact-1 is replaced once, by
	// the earliest later fact that made it stale, not by both of them.
	byFact := map[string][]Supersede{}
	for _, finding := range plan.Supersedes {
		byFact[finding.FactID] = append(byFact[finding.FactID], finding)
	}
	if len(byFact["fact-1"]) != 1 {
		t.Fatalf("fact-1 has %d findings: %#v", len(byFact["fact-1"]), byFact["fact-1"])
	}
	if byFact["fact-1"][0].SupersededBy != "fact-2" {
		t.Fatalf("fact-1 names %q, want the earliest replacer", byFact["fact-1"][0].SupersededBy)
	}
}

// TestDetectUnansweredPairsStayCurrent pins the other side of the threshold: a
// pair the arbiter leaves undecided becomes no finding, is counted as asked
// and undecided, and changes nothing about the fact it could not decide.
func TestDetectUnansweredPairsStayCurrent(t *testing.T) {
	stub := &stubArbiter{answer: func(Pair) Judgment { return Judgment{Verdict: VerdictUndecided} }}
	config := Config{Arbiter: stub}
	plan, pairs, err := detect(
		corememory.Scope{RuntimeID: "runtime", UserID: "user"},
		undecidedFacts(
			fact("fact-1", "郑凯住在柏林。", []string{"郑凯", "柏林"}, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)),
			fact("fact-2", "郑凯搬去了巴黎。", []string{"郑凯", "巴黎"}, time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)),
		), time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := adjudicate(context.Background(), &plan, pairs, stub); err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 0 {
		t.Fatalf("supersedes = %#v", plan.Supersedes)
	}
	if plan.ArbiterAsked != 1 || plan.ArbiterUndecided != 1 {
		t.Fatalf("counts = asked %d, undecided %d", plan.ArbiterAsked, plan.ArbiterUndecided)
	}
}

// TestDetectIsDeterministicWithAnArbiter pins what the overlay's stamp
// promises: two passes over the same facts propose the same pairs to the same
// arbiter and write the same findings, so a plan is a function of the scope and
// not of the order a map happened to iterate in.
func TestDetectIsDeterministicWithAnArbiter(t *testing.T) {
	conversations := func() map[string][]factview.Fact {
		return map[string][]factview.Fact{
			"conv-1": {
				fact("fact-1", "郑凯住在柏林。", []string{"郑凯", "柏林"}, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)),
				fact("fact-2", "郑凯搬去了巴黎。", []string{"郑凯", "巴黎"}, time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)),
			},
			"conv-2": {
				fact("fact-3", "李娜住在广州。", []string{"李娜", "广州"}, time.Date(2021, 3, 1, 0, 0, 0, 0, time.UTC)),
				fact("fact-4", "李娜搬去了上海。", []string{"李娜", "上海"}, time.Date(2022, 4, 1, 0, 0, 0, 0, time.UTC)),
			},
		}
	}
	first, firstStub := detectWith(t, Config{}, conversations())
	second, secondStub := detectWith(t, Config{}, conversations())
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("plans differ:\n%s\n%s", firstJSON, secondJSON)
	}
	for index := range firstStub.asked {
		if firstStub.asked[index].Older.ID != secondStub.asked[index].Older.ID ||
			firstStub.asked[index].Newer.ID != secondStub.asked[index].Newer.ID {
			t.Fatalf("pair %d differs: %s->%s vs %s->%s", index,
				firstStub.asked[index].Older.ID, firstStub.asked[index].Newer.ID,
				secondStub.asked[index].Older.ID, secondStub.asked[index].Newer.ID)
		}
	}
}

// TestRunScopeReportsAFailedArbiterAndKeepsItsRules pins the failure shape: a
// pass that cannot reach the arbiter still publishes what its own rules found
// -- the overlay is what the read path applies -- and reports the missing half
// to the caller instead of passing its silence off as a judgment.
func TestRunScopeReportsAFailedArbiterAndKeepsItsRules(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := factview.NewFactStore(logStore, kv)
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubArbiter{err: errors.New("provider unavailable")}
	// One tuning for both halves: the store serves only what a pass with its
	// own stamp wrote, and the stamp carries the arbiter's identity.
	config := Config{Arbiter: stub}
	store, err := NewStore(kv, WithConfig(config))
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	seed := func(id, text, predicate string, entities []string, at time.Time) {
		t.Helper()
		if _, err := facts.Add(ctx, factview.AddRequest{
			ID: id, Generation: "generation-1", Scope: scope, ConversationID: "conv-1",
			Content:    coremessage.NewTextContent(text),
			Entities:   entities,
			Predicate:  predicate,
			EventTime:  at,
			Provenance: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message-" + id}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// What the vocabulary decides, and what only the arbiter could.
	seed("fact-1", "Caroline lives in Berlin.", "lives in", []string{"Caroline", "Berlin"}, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
	seed("fact-2", "Caroline lives in Paris.", "lives in", []string{"Caroline", "Paris"}, time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC))
	seed("fact-3", "郑凯住在柏林。", "", []string{"郑凯", "柏林"}, time.Date(2021, 2, 1, 0, 0, 0, 0, time.UTC))
	seed("fact-4", "郑凯上个月搬到了上海。", "", []string{"郑凯"}, time.Date(2022, 2, 1, 0, 0, 0, 0, time.UTC))
	if err := facts.PublishActiveGeneration(ctx, scope, "conv-1", "generation-1"); err != nil {
		t.Fatal(err)
	}

	service := &Service{Facts: facts, Store: store, Config: config}
	plan, err := service.RunScope(ctx, scope)
	if err == nil {
		t.Fatal("a failed arbiter did not fail the pass")
	}
	if len(plan.Supersedes) != 1 || plan.Supersedes[0].Reason != ReasonRelation {
		t.Fatalf("plan kept no rule finding: %#v", plan.Supersedes)
	}
	status, err := store.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Stored {
		t.Fatal("the pass did not publish the overlay its rules found")
	}
}

// TestRunScopeStoresTheArbitersEvidence pins what the overlay keeps: the
// finding's reason and probability are written with the factor, so a host
// reading an overlay can explain a penalty without re-running the pass.
func TestRunScopeStoresTheArbitersEvidence(t *testing.T) {
	ctx := context.Background()
	ws, err := workspace.NewLocalWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	kv, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := factview.NewFactStore(logStore, kv)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Arbiter: &stubArbiter{answer: alwaysReplaced}}
	store, err := NewStore(kv, WithConfig(config))
	if err != nil {
		t.Fatal(err)
	}
	scope := corememory.Scope{RuntimeID: "runtime", UserID: "user"}
	seed := func(id, text string, entities []string, at time.Time) {
		t.Helper()
		if _, err := facts.Add(ctx, factview.AddRequest{
			ID: id, Generation: "generation-1", Scope: scope, ConversationID: "conv-1",
			Content:    coremessage.NewTextContent(text),
			Entities:   entities,
			EventTime:  at,
			Provenance: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "message-" + id}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seed("fact-1", "郑凯住在柏林。", []string{"郑凯", "柏林"}, time.Date(2021, 5, 1, 0, 0, 0, 0, time.UTC))
	seed("fact-2", "郑凯上个月搬到了上海。", []string{"郑凯"}, time.Date(2023, 8, 14, 0, 0, 0, 0, time.UTC))
	if err := facts.PublishActiveGeneration(ctx, scope, "conv-1", "generation-1"); err != nil {
		t.Fatal(err)
	}

	service := &Service{Facts: facts, Store: store, Config: config}
	plan, err := service.RunScope(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Supersedes) != 1 || plan.Supersedes[0].Reason != ReasonArbiter {
		t.Fatalf("plan = %#v", plan.Supersedes)
	}
	identity, err := itemKey(scope, "conv-1", "fact-1")
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := store.Load(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := overlay[identity]
	if !ok {
		t.Fatalf("overlay = %#v", overlay)
	}
	if entry.Reason != ReasonArbiter || entry.Confidence != 0.93 || entry.SupersededBy != "fact-2" {
		t.Fatalf("entry = %#v", entry)
	}
}

// TestConfigValidatesTheArbiterBudget pins the one tuning the arbiter block
// adds to the pass: a negative budget is a configuration error rather than an
// arbiter that is never asked.
func TestConfigValidatesTheArbiterBudget(t *testing.T) {
	_, err := DetectWithConfig(
		corememory.Scope{RuntimeID: "runtime", UserID: "user"},
		nil, time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC), Config{ArbiterBudget: -1})
	if err == nil {
		t.Fatal("a negative arbiter budget was accepted")
	}
}
