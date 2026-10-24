package eval

import (
	"bytes"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
)

const loCoMoFixture = `[
  {
    "sample_id": "sample-1",
    "conversation": {
      "speaker_a": "Alice",
      "speaker_b": "Bob",
      "session_2_date_time": "2:00 pm on 2 May, 2023",
      "session_2": [
        {"speaker": "Bob", "dia_id": "D2:1", "text": "I adopted a dog."}
      ],
      "session_1_date_time": "1:00 pm on 1 May, 2023",
      "session_1": [
        {"speaker": "Alice", "dia_id": "D1:1", "text": "I like tea."},
        {"speaker": "Bob", "dia_id": "D1:2", "text": "Nice.", "query": "a tea cup", "img_url": ["https://example.test/cup.png"]}
      ]
    },
    "qa": [
      {"question": "What does Alice like?", "answer": "tea", "evidence": ["D1:1"], "category": 1},
      {"question": "What pet?", "answer": null, "category": 2},
      {"question": "What did Alice realize after the race?", "answer": null, "evidence": ["D9:9"], "category": 5}
    ]
  }
]`

// longMemEvalLabelsFixture is the two shapes the label rules have to keep
// apart: an abstention row ("_abs", where refusing is the answer) and a type
// this build does not know.
const longMemEvalLabelsFixture = `[
  {
    "question_id": "q-labels_abs",
    "question_type": "temporal-reasoning",
    "question": "Which task did I complete first?",
    "answer": "The information provided is not enough.",
    "question_date": "2023/07/02 (Sun) 09:15",
    "answer_session_ids": ["s1"],
    "haystack_session_ids": ["s1"],
    "haystack_dates": ["2023/05/01 (Mon) 09:00"],
    "haystack_sessions": [[{"role": "user", "content": "I like tea."}]]
  },
  {
    "question_id": "q-new-type",
    "question_type": "brand-new-type",
    "question": "What is my favourite colour?",
    "answer": "blue",
    "question_date": "2023/07/02 (Sun) 09:15",
    "answer_session_ids": [],
    "haystack_session_ids": ["s1"],
    "haystack_dates": ["2023/05/01 (Mon) 09:00"],
    "haystack_sessions": [[{"role": "user", "content": "I like blue."}]]
  }
]`

const longMemEvalFixture = `[
  {
    "question_id": "q-1",
    "question_type": "temporal",
    "question": "When did I adopt the dog?",
    "answer": "May 2023",
    "question_date": "2023-06-01",
    "haystack_session_ids": ["s2", "s1"],
    "haystack_dates": ["2023-05-02", "2023-05-01"],
    "haystack_sessions": [
      [{"role": "assistant", "content": "Congrats on the dog!"}],
      [{"role": "user", "content": "I adopted a dog today.", "has_answer": true}]
    ]
  }
]`

func TestLoadLoCoMo(t *testing.T) {
	scenarios, stats, err := LoadLoCoMo([]byte(loCoMoFixture), LoaderOptions{Scope: Scope{RuntimeID: "eval"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 1 || stats.Conversations != 1 || stats.Questions != 1 || stats.Skipped != 1 {
		t.Fatalf("scenarios=%d stats=%#v", len(scenarios), stats)
	}
	if stats.SkippedAdversarial != 1 {
		t.Fatalf("adversarial questions must be excluded and counted separately: stats=%#v", stats)
	}
	scenario := scenarios[0]
	if len(scenario.Questions) != 1 {
		t.Fatalf("adversarial question leaked into the graded set: %#v", scenario.Questions)
	}
	if len(scenario.Turns) != 2 {
		t.Fatalf("turns = %d, want 2 sessions", len(scenario.Turns))
	}
	first := scenario.Turns[0]
	if first.Messages[0].Role != message.RoleUser {
		t.Fatalf("first role = %q", first.Messages[0].Role)
	}
	if got := first.Messages[1].Content.Text(); !bytes.Contains([]byte(got), []byte("[shared image: a tea cup]")) {
		t.Fatalf("image annotation missing: %q", got)
	}
	if scenario.Questions[0].WantContains[0] != "tea" || scenario.Questions[0].Category != 1 {
		t.Fatalf("question = %#v", scenario.Questions[0])
	}
	if evidence := scenario.Questions[0].Evidence; len(evidence) != 1 || evidence[0] != "D1:1" {
		t.Fatalf("question evidence = %#v", evidence)
	}
	if ids := scenario.Turns[0].DatasetIDs; len(ids) != 2 || ids[0] != "D1:1" || ids[1] != "D1:2" {
		t.Fatalf("turn dataset ids = %#v", ids)
	}
	if len(scenario.Turns[1].Messages) != len(scenario.Turns[1].DatasetIDs) {
		t.Fatalf("dataset ids must align with messages: %#v", scenario.Turns[1])
	}
}

// The session date prefix is a rendering choice rather than dataset content:
// the dateless conversion drops exactly the "[<date>] " prefix while speaker,
// body, dataset ids and the image annotation stay identical, so two stores
// built from the two arms hold the same turns up to this one step.
func TestLoadLoCoMoDatePrefix(t *testing.T) {
	options := LoaderOptions{Scope: Scope{RuntimeID: "eval"}}
	prefixed, _, err := LoadLoCoMo([]byte(loCoMoFixture), options)
	if err != nil {
		t.Fatal(err)
	}
	if got := prefixed[0].Turns[0].Messages[0].Content.Text(); got != "[1:00 pm on 1 May, 2023] Alice: I like tea." {
		t.Fatalf("prefixed text = %q", got)
	}
	options.Dateless = true
	dateless, _, err := LoadLoCoMo([]byte(loCoMoFixture), options)
	if err != nil {
		t.Fatal(err)
	}
	if got := dateless[0].Turns[0].Messages[0].Content.Text(); got != "Alice: I like tea." {
		t.Fatalf("dateless text = %q", got)
	}
	if got := dateless[0].Turns[1].Messages[0].Content.Text(); got != "Bob: I adopted a dog." {
		t.Fatalf("dateless text = %q", got)
	}
	if got := dateless[0].Turns[0].Messages[1].Content.Text(); got != "Bob: Nice. [shared image: a tea cup]" {
		t.Fatalf("dateless image turn = %q", got)
	}
	if ids := dateless[0].Turns[0].DatasetIDs; len(ids) != 2 || ids[0] != "D1:1" || ids[1] != "D1:2" {
		t.Fatalf("dateless dataset ids = %#v", ids)
	}
	if len(dateless[0].Turns) != len(prefixed[0].Turns) {
		t.Fatalf("turn count changed: %d -> %d", len(prefixed[0].Turns), len(dateless[0].Turns))
	}
}

func TestLoadLongMemEvalOrdersSessions(t *testing.T) {
	scenarios, stats, err := LoadLongMemEval([]byte(longMemEvalFixture), LoaderOptions{Scope: Scope{RuntimeID: "eval"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 1 || stats.Turns != 2 || stats.Questions != 1 {
		t.Fatalf("scenarios=%d stats=%#v", len(scenarios), stats)
	}
	scenario := scenarios[0]
	if scenario.Turns[0].IdempotencyKey != "longmemeval/q-1/s1" {
		t.Fatalf("session order = %q", scenario.Turns[0].IdempotencyKey)
	}
	if scenario.Questions[0].WantContains[0] != "May 2023" {
		t.Fatalf("question = %#v", scenario.Questions[0])
	}
}

// longMemEvalDatedFixture carries three sessions out of date order, one of them
// with a date string no layout parses. It is the shape the dating rules have to
// answer: which session happened when, and what happens when the date is
// garbage.
const longMemEvalDatedFixture = `[
  {
    "question_id": "q-dates",
    "question_type": "temporal-reasoning",
    "question": "How long after I adopted the dog did I move?",
    "answer": "two months",
    "question_date": "2023/07/02 (Sun) 09:15",
    "answer_session_ids": ["s-b", "s-a", "s-unknown"],
    "haystack_session_ids": ["s-b", "s-a", "s-c"],
    "haystack_dates": ["2023/06/20 (Tue) 08:00", "2023/05/01 (Mon) 09:00", "not a date"],
    "haystack_sessions": [
      [{"role": "user", "content": "We moved to Lisbon."}, {"role": "assistant", "content": "Congrats!"}],
      [{"role": "user", "content": "I adopted a dog today."}],
      [{"role": "user", "content": "I like tea."}]
    ]
  }
]`

// The session date is what a temporal question resolves against: it reaches the
// messages as the observed_at anchor, its whole session shares it, and a date
// that cannot be parsed leaves the messages unanchored instead of guessing.
func TestLoadLongMemEvalAnchorsSessionDates(t *testing.T) {
	options := LoaderOptions{Scope: Scope{RuntimeID: "eval"}, Dateless: true, ObservedAt: "metadata"}
	scenarios, stats, err := LoadLongMemEval([]byte(longMemEvalDatedFixture), options)
	if err != nil {
		t.Fatal(err)
	}
	turns := scenarios[0].Turns
	if len(turns) != 3 {
		t.Fatalf("turns = %d", len(turns))
	}
	if turns[0].IdempotencyKey != "longmemeval/q-dates/s-a" || turns[1].IdempotencyKey != "longmemeval/q-dates/s-b" {
		t.Fatalf("sessions are not in date order: %q, %q", turns[0].IdempotencyKey, turns[1].IdempotencyKey)
	}
	if got := turns[0].ObservedAt; len(got) != 1 || got[0] != "2023-05-01T09:00:00Z" {
		t.Fatalf("s-a observed_at = %#v", got)
	}
	// The anchor is a property of the session, so every message in it carries
	// the same value.
	if got := turns[1].ObservedAt; len(got) != 2 || got[0] != "2023-06-20T08:00:00Z" || got[1] != "2023-06-20T08:00:00Z" {
		t.Fatalf("s-b observed_at = %#v", got)
	}
	if got := turns[2].ObservedAt; len(got) != 0 {
		t.Fatalf("an unparsable date must leave the messages unanchored: %#v", got)
	}
	if stats.ObservedAtAnchored != 3 || stats.ObservedAtUnparsed != 1 {
		t.Fatalf("observed-at stats = %#v", stats)
	}
}

// The text prefix is the other rendering of the same session date, and the two
// are alternatives: the metadata anchor requires the dateless conversion, the
// same rule the LoCoMo conversion enforces.
func TestLoadLongMemEvalDatePrefix(t *testing.T) {
	prefixed, _, err := LoadLongMemEval([]byte(longMemEvalDatedFixture), LoaderOptions{Scope: Scope{RuntimeID: "eval"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := prefixed[0].Turns[0].Messages[0].Content.Text(); got != "[2023/05/01 (Mon) 09:00] I adopted a dog today." {
		t.Fatalf("prefixed text = %q", got)
	}
	dateless, _, err := LoadLongMemEval([]byte(longMemEvalDatedFixture),
		LoaderOptions{Scope: Scope{RuntimeID: "eval"}, Dateless: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := dateless[0].Turns[0].Messages[0].Content.Text(); got != "I adopted a dog today." {
		t.Fatalf("dateless text = %q", got)
	}
	if _, _, err := LoadLongMemEval([]byte(longMemEvalDatedFixture),
		LoaderOptions{Scope: Scope{RuntimeID: "eval"}, ObservedAt: "metadata"}); err == nil {
		t.Fatal("observed_at=metadata without the dateless conversion was accepted")
	}
}

// The dataset labels its evidence by session, so the question carries the gold
// sessions while every message still gets an id: that id is what ingest writes
// and what lets a recalled message be matched back to its session. A gold
// session the haystack does not hold is counted, not graded as a permanent miss.
func TestLoadLongMemEvalNamesEvidenceSessionsAndTurnIDs(t *testing.T) {
	scenarios, stats, err := LoadLongMemEval([]byte(longMemEvalDatedFixture), LoaderOptions{Scope: Scope{RuntimeID: "eval"}})
	if err != nil {
		t.Fatal(err)
	}
	question := scenarios[0].Questions[0]
	if got := question.EvidenceSessions; len(got) != 2 || got[0] != "s-b" || got[1] != "s-a" {
		t.Fatalf("evidence sessions = %#v", got)
	}
	if len(question.Evidence) != 0 {
		t.Fatalf("session evidence must not be restated as turn evidence: %#v", question.Evidence)
	}
	if stats.EvidenceUnresolved != 1 {
		t.Fatalf("an unresolvable gold session must be counted: stats=%#v", stats)
	}
	turns := scenarios[0].Turns
	if got := turns[0].DatasetIDs; len(got) != 1 || got[0] != "s-a#0" {
		t.Fatalf("s-a dataset ids = %#v", got)
	}
	if got := turns[1].DatasetIDs; len(got) != 2 || got[0] != "s-b#0" || got[1] != "s-b#1" {
		t.Fatalf("s-b dataset ids = %#v", got)
	}
	if len(turns[1].DatasetIDs) != len(turns[1].Messages) {
		t.Fatalf("dataset ids must align with messages: %#v", turns[1])
	}
}

// Every row the dataset marks is carried into the question: the asked-at date,
// the type as both a report bucket and its own label, and the "_abs" marker
// that says the graded behaviour is a refusal. A type this build does not know
// keeps its label and lands in the catch-all bucket rather than reading as
// uncategorised.
func TestLoadLongMemEvalCarriesQuestionLabels(t *testing.T) {
	scenarios, _, err := LoadLongMemEval([]byte(longMemEvalLabelsFixture), LoaderOptions{Scope: Scope{RuntimeID: "eval"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(scenarios) != 2 {
		t.Fatalf("scenarios = %d", len(scenarios))
	}
	dated := scenarios[0].Questions[0]
	if dated.AskedAt != "2023/07/02 (Sun) 09:15" {
		t.Fatalf("asked_at = %q, want the dataset's own string", dated.AskedAt)
	}
	if dated.Category != LongMemEvalTemporalReasoning || dated.CategoryLabel != "temporal-reasoning" {
		t.Fatalf("category = %d %q", dated.Category, dated.CategoryLabel)
	}
	if !dated.Unanswerable {
		t.Fatal("the _abs row must be marked unanswerable")
	}
	// The one thing the category id must never be is LoCoMo's temporal id: that
	// number routes the reference harness's question suffix.
	if dated.Category == TemporalCategory {
		t.Fatalf("LongMemEval temporal-reasoning shares LoCoMo's category %d", TemporalCategory)
	}
	unknown := scenarios[1].Questions[0]
	if unknown.Category != LongMemEvalUnknownCategory || unknown.CategoryLabel != "brand-new-type" {
		t.Fatalf("unknown type = %d %q", unknown.Category, unknown.CategoryLabel)
	}
	if unknown.Unanswerable {
		t.Fatal("a question without the _abs marker must stay answerable")
	}
}

func TestLongMemEvalTurnIDsRoundTrip(t *testing.T) {
	if got := LongMemEvalTurnID("answer_4be1b6b4_2", 3); got != "answer_4be1b6b4_2#3" {
		t.Fatalf("turn id = %q", got)
	}
	for _, test := range []struct {
		id      string
		session string
		ok      bool
	}{
		{"answer_4be1b6b4_2#3", "answer_4be1b6b4_2", true},
		{"a#b#7", "a#b", true},
		{"D1:3", "", false},
		{"#3", "", false},
		{"s-a#", "", false},
	} {
		session, ok := LongMemEvalSessionOfTurn(test.id)
		if ok != test.ok || session != test.session {
			t.Fatalf("%q -> (%q, %v), want (%q, %v)", test.id, session, ok, test.session, test.ok)
		}
	}
}

func TestBaselineCompareAndRoundTrip(t *testing.T) {
	baseline := NewBaseline("nightly", []Report{
		{Scenario: "locomo/sample-1", HitRate: 1},
		{Scenario: "locomo/sample-2", HitRate: 0.5},
	})
	regressions := baseline.Compare([]Report{
		{Scenario: "locomo/sample-1", HitRate: 0.5},
		{Scenario: "locomo/sample-2", HitRate: 0.45},
		{Scenario: "locomo/sample-3", HitRate: 0},
	}, 0.1)
	if len(regressions) != 1 || regressions[0].Scenario != "locomo/sample-1" {
		t.Fatalf("regressions = %#v", regressions)
	}
	var buffer bytes.Buffer
	if err := SaveBaseline(&buffer, baseline); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadBaseline(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Reports) != 2 || loaded.Name != "nightly" {
		t.Fatalf("loaded = %#v", loaded)
	}
}

// The observed-at conversion moves the session date out of the text and into
// the message metadata: the text stays dateless, the anchor is the session
// date as RFC3339 UTC, and the dataset ids are untouched. This is the shape a
// prefix-less production path is measured through.
func TestLoadLoCoMoObservedAtMetadata(t *testing.T) {
	options := LoaderOptions{Scope: Scope{RuntimeID: "eval"}, Dateless: true, ObservedAt: "metadata"}
	scenarios, stats, err := LoadLoCoMo([]byte(loCoMoFixture), options)
	if err != nil {
		t.Fatal(err)
	}
	first := scenarios[0].Turns[0]
	if got := first.Messages[0].Content.Text(); got != "Alice: I like tea." {
		t.Fatalf("metadata conversion changed the text: %q", got)
	}
	if got := first.ObservedAt; len(got) != 2 || got[0] != "2023-05-01T13:00:00Z" || got[1] != "2023-05-01T13:00:00Z" {
		t.Fatalf("observed_at = %#v", got)
	}
	if got := scenarios[0].Turns[1].ObservedAt; len(got) != 1 || got[0] != "2023-05-02T14:00:00Z" {
		t.Fatalf("second session observed_at = %#v", got)
	}
	if stats.ObservedAtAnchored != 3 || stats.ObservedAtUnparsed != 0 {
		t.Fatalf("observed-at stats = %#v", stats)
	}
}

// The metadata anchor replaces the text prefix; asking for both is rejected
// here rather than resolved silently into a turn that carries two anchors.
func TestLoadLoCoMoObservedAtRequiresDateless(t *testing.T) {
	_, _, err := LoadLoCoMo([]byte(loCoMoFixture), LoaderOptions{Scope: Scope{RuntimeID: "eval"}, ObservedAt: "metadata"})
	if err == nil {
		t.Fatal("observed_at=metadata without the dateless conversion was accepted")
	}
}

// A session date the conversion cannot parse leaves the messages without an
// anchor and is counted, so anchor coverage is a reading on the run rather
// than an assumption.
func TestLoadLoCoMoObservedAtUnparsableDate(t *testing.T) {
	fixture := strings.Replace(loCoMoFixture, "1:00 pm on 1 May, 2023", "sometime in May", 1)
	scenarios, stats, err := LoadLoCoMo([]byte(fixture),
		LoaderOptions{Scope: Scope{RuntimeID: "eval"}, Dateless: true, ObservedAt: "metadata"})
	if err != nil {
		t.Fatal(err)
	}
	if got := scenarios[0].Turns[0].ObservedAt; got != nil {
		t.Fatalf("unparsable date produced anchors: %#v", got)
	}
	if stats.ObservedAtUnparsed != 1 || stats.ObservedAtAnchored != 1 {
		t.Fatalf("observed-at stats = %#v", stats)
	}
}
