package scenarios

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
)

func TestConvertLongMemEvalRealShape(t *testing.T) {
	raw := []byte(`[
		{
			"question_id": "gpt4_abc",
			"question_type": "temporal-reasoning",
			"question": "What was the first issue?",
			"answer": "GPS system not functioning correctly",
			"question_date": "2023/04/10 (Mon) 23:07",
			"haystack_dates": [
				"2023/04/10 (Mon) 17:50",
				"2023/04/10 (Mon) 14:47",
				"2023/04/10 (Mon) 17:15"
			],
			"haystack_session_ids": ["answer_x_2", "answer_x_3", "answer_x_1"],
			"haystack_sessions": [
				[{"role": "user", "content": "late session", "has_answer": false}],
				[{"role": "user", "content": "early evidence turn", "has_answer": true}],
				[{"role": "assistant", "content": "middle session", "has_answer": false}]
			],
			"answer_session_ids": ["answer_x_3"]
		},
		{
			"question_id": "gpt4_abs_num",
			"question_type": "knowledge-update",
			"question": "How many children?",
			"answer": 3,
			"question_date": "2023/04/11 (Tue) 10:00",
			"haystack_dates": ["2023/04/11 (Tue) 09:00"],
			"haystack_session_ids": ["answer_y_1"],
			"haystack_sessions": [[{"role": "user", "content": "three", "has_answer": true}]],
			"answer_session_ids": ["answer_y_1"]
		}
	]`)
	converted, stats, err := (longmemevalScenario{}).Convert(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(converted.Conversations) != 2 || stats.Conversations != 2 {
		t.Fatalf("conversations = %d, stats = %+v", len(converted.Conversations), stats)
	}
	first := converted.Conversations[0]
	if len(first.Turns) != 3 {
		t.Fatalf("first turns = %d", len(first.Turns))
	}
	if got := first.Turns[0].Content.Text(); !strings.Contains(got, "[2023/04/10 (Mon) 14:47] user: early evidence turn") {
		t.Fatalf("sessions not sorted chronologically: %q", got)
	}
	if first.Turns[1].Role != message.RoleAssistant {
		t.Fatalf("middle role = %q", first.Turns[1].Role)
	}
	question := converted.Questions[0]
	if question.AskedAt != "2023/04/10 (Mon) 23:07" {
		t.Fatalf("asked_at = %q", question.AskedAt)
	}
	if len(question.EvidenceIDs) != 1 || question.EvidenceIDs[0] != "gpt4_abc:answer_x_3:t0" {
		t.Fatalf("evidence = %#v", question.EvidenceIDs)
	}
	second := converted.Questions[1]
	if second.GoldAnswers[0] != "3" {
		t.Fatalf("number gold = %#v", second.GoldAnswers)
	}
	if !strings.Contains(second.ID, "_abs") {
		t.Fatalf("abs tag missing: %#v", second.Tags)
	}
}
