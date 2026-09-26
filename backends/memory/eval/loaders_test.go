package eval

import (
	"bytes"
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
