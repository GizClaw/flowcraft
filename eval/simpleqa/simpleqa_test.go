package simpleqa_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/eval/simpleqa"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// scriptedLLM is a deterministic fake that returns a pre-written reply
// for each call in order. We use it to drive Run end-to-end without
// touching a real LLM provider — the contract under test is the
// aggregator + judge-parser, not the model.
type scriptedLLM struct {
	replies []string
	idx     int
}

func (s *scriptedLLM) Generate(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	if s.idx >= len(s.replies) {
		return llm.Message{}, llm.TokenUsage{}, nil
	}
	reply := s.replies[s.idx]
	s.idx++
	return model.NewTextMessage(model.RoleAssistant, reply), llm.TokenUsage{}, nil
}

func (s *scriptedLLM) GenerateStream(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}

// TestLoadDataset_CSV verifies the CSV path handles the upstream
// 3-column layout (problem, answer, metadata) and lifts metadata
// fields into typed columns.
func TestLoadDataset_CSV(t *testing.T) {
	dir := t.TempDir()
	csvPath := filepath.Join(dir, "test.csv")
	body := `problem,answer,metadata
"What is 2+2?","4","{""topic"": ""Math"", ""answer_type"": ""number""}"
"Capital of France?","Paris","{""topic"": ""Geography""}"
`
	if err := os.WriteFile(csvPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ds, err := simpleqa.LoadDataset(csvPath)
	if err != nil {
		t.Fatalf("LoadDataset: %v", err)
	}
	if len(ds.Questions) != 2 {
		t.Fatalf("want 2 questions, got %d", len(ds.Questions))
	}
	if got, want := ds.Questions[0].Problem, "What is 2+2?"; got != want {
		t.Errorf("Q1 problem: want %q, got %q", want, got)
	}
	if got, want := ds.Questions[0].Topic, "Math"; got != want {
		t.Errorf("Q1 topic: want %q, got %q", want, got)
	}
	if got, want := ds.Questions[0].AnswerType, "number"; got != want {
		t.Errorf("Q1 answer_type: want %q, got %q", want, got)
	}
	if got, want := ds.Questions[1].Topic, "Geography"; got != want {
		t.Errorf("Q2 topic: want %q, got %q", want, got)
	}
}

// TestLoadDataset_JSONL verifies the JSONL path round-trips fields
// through the Question struct without dropping data.
func TestLoadDataset_JSONL(t *testing.T) {
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "test.jsonl")
	body := `{"id":"q1","problem":"What is 2+2?","answer":"4","topic":"Math"}
{"id":"q2","problem":"Capital of France?","answer":"Paris","topic":"Geography"}
`
	if err := os.WriteFile(jsonlPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	ds, err := simpleqa.LoadDataset(jsonlPath)
	if err != nil {
		t.Fatalf("LoadDataset: %v", err)
	}
	if len(ds.Questions) != 2 {
		t.Fatalf("want 2 questions, got %d", len(ds.Questions))
	}
	if got := ds.Questions[0].ID; got != "q1" {
		t.Errorf("Q1 id: want %q, got %q", "q1", got)
	}
}

// TestRun_AllVerdicts walks all three verdict buckets in one scripted
// run: Q1 correct, Q2 incorrect, Q3 abstained. The aggregator must
// produce N=3, Correct=1, Incorrect=1, NotAttempted=1, Accuracy=1/3,
// AttemptedAccuracy=1/2, AbstentionRate=1/3, HallucinationRate=1/3.
func TestRun_AllVerdicts(t *testing.T) {
	ds := &simpleqa.Dataset{
		Name: "synth",
		Questions: []simpleqa.Question{
			{ID: "q1", Problem: "What is 2+2?", Answer: "4", Topic: "Math"},
			{ID: "q2", Problem: "Capital of France?", Answer: "Paris", Topic: "Geography"},
			{ID: "q3", Problem: "Atomic weight of X?", Answer: "12", Topic: "Science"},
		},
	}
	// Replies alternate: answer, judge, answer, judge, …
	answer := &scriptedLLM{replies: []string{"4", "London", "I don't know"}}
	judge := &scriptedLLM{replies: []string{"A", "B", "C"}}

	rep, err := simpleqa.Run(context.Background(), ds, simpleqa.Options{
		AnswerLLM:             answer,
		JudgeLLM:              judge,
		Concurrency:           1, // sequential so scripted replies line up deterministically
		IncludeTopicBreakdown: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if rep.N != 3 {
		t.Errorf("N: want 3, got %d", rep.N)
	}
	if rep.Correct != 1 || rep.Incorrect != 1 || rep.NotAttempted != 1 {
		t.Errorf("verdicts: correct=%d incorrect=%d not_attempted=%d (want 1/1/1)",
			rep.Correct, rep.Incorrect, rep.NotAttempted)
	}
	if rep.Accuracy != 1.0/3.0 {
		t.Errorf("Accuracy: want %.4f, got %.4f", 1.0/3.0, rep.Accuracy)
	}
	if rep.AttemptedAccuracy != 0.5 {
		t.Errorf("AttemptedAccuracy: want 0.5, got %.4f", rep.AttemptedAccuracy)
	}
	if rep.AbstentionRate != 1.0/3.0 {
		t.Errorf("AbstentionRate: want %.4f, got %.4f", 1.0/3.0, rep.AbstentionRate)
	}
	if rep.HallucinationRate != 1.0/3.0 {
		t.Errorf("HallucinationRate: want %.4f, got %.4f", 1.0/3.0, rep.HallucinationRate)
	}
	if got := len(rep.PerTopic); got != 3 {
		t.Errorf("PerTopic: want 3 topics, got %d", got)
	}
}

// TestRun_HookEventsOrdering pins the lifecycle sequence so the Feishu
// card renderer doesn't silently regress.
func TestRun_HookEventsOrdering(t *testing.T) {
	ds := &simpleqa.Dataset{
		Name: "synth",
		Questions: []simpleqa.Question{
			{ID: "q1", Problem: "p?", Answer: "a"},
		},
	}
	answer := &scriptedLLM{replies: []string{"a"}}
	judge := &scriptedLLM{replies: []string{"A"}}

	var kinds []string
	_, err := simpleqa.Run(context.Background(), ds, simpleqa.Options{
		AnswerLLM:   answer,
		JudgeLLM:    judge,
		Concurrency: 1,
		Hook: func(_ context.Context, e simpleqa.Event) {
			kinds = append(kinds, e.Kind)
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []string{"start", "done"}
	if len(kinds) != len(want) {
		t.Fatalf("events: want %v, got %v", want, kinds)
	}
	for i, w := range want {
		if kinds[i] != w {
			t.Errorf("event[%d]: want %q, got %q", i, w, kinds[i])
		}
	}
}
