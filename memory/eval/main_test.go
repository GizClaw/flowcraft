package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

func TestConvertLoCoMoRealShape(t *testing.T) {
	raw := []byte(`[
		{
			"sample_id": "conv-26",
			"conversation": {
				"speaker_a": "Caroline",
				"speaker_b": "Melanie",
				"session_1_date_time": "1:56 pm on 8 May, 2023",
				"session_1": [
					{"speaker": "Caroline", "dia_id": "D1:1", "text": "Hello Mel!"},
					{"speaker": "Melanie", "dia_id": "D1:2", "text": "Hey Caroline!"},
					{"speaker": "Caroline", "dia_id": "D1:3", "text": "I went to a support group yesterday."},
					{"speaker": "Caroline", "dia_id": "D1:4", "text": "Look at this mural.",
					 "img_url": ["https://example.invalid/x.png"], "query": "transgender pride flag mural",
					 "blip_caption": "a photo of a dog"}
				]
			},
			"qa": [
				{"question": "When did Caroline go to the support group?", "answer": "7 May 2023",
				 "evidence": ["D1:3"], "category": 2},
				{"question": "What has Melanie painted?", "answer": 2022,
				 "evidence": ["D1:3; D1:4"], "category": 1},
				{"question": "Broken evidence row?", "answer": "x",
				 "evidence": ["D1:3", "D", "D1:99"], "category": 4},
				{"question": "No gold answer?", "answer": null, "evidence": [], "category": 5}
			]
		}
	]`)
	dataset, stats, err := convertLoCoMo(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(dataset.Conversations) != 1 {
		t.Fatalf("conversations = %d", len(dataset.Conversations))
	}
	conversation := dataset.Conversations[0]
	if conversation.ID != "conv-26" || len(conversation.Turns) != 4 {
		t.Fatalf("conversation = %#v", conversation)
	}
	if got := conversation.Turns[0].Content; !strings.Contains(got, "[1:56 pm on 8 May, 2023] Caroline: Hello Mel!") {
		t.Fatalf("turn content = %q", got)
	}
	if conversation.Turns[1].Role != "assistant" {
		t.Fatalf("speaker_b role = %q", conversation.Turns[1].Role)
	}
	if got := conversation.Turns[3].Content; !strings.Contains(got, "[shared image: transgender pride flag mural]") {
		t.Fatalf("image annotation = %q", got)
	}
	if len(dataset.Questions) != 3 {
		t.Fatalf("questions = %d, want 3 (null-answer row skipped)", len(dataset.Questions))
	}
	if got := dataset.Questions[0].EvidenceIDs; len(got) != 1 || got[0] != "D1:3" {
		t.Fatalf("evidence[0] = %#v, want [D1:3] without prefix", got)
	}
	if got := dataset.Questions[1].EvidenceIDs; len(got) != 2 || got[0] != "D1:3" || got[1] != "D1:4" {
		t.Fatalf("semicolon evidence = %#v", got)
	}
	if got := dataset.Questions[2].EvidenceIDs; len(got) != 1 || got[0] != "D1:3" {
		t.Fatalf("invalid evidence not dropped: %#v", got)
	}
	if dataset.Questions[1].GoldAnswers[0] != "2022" {
		t.Fatalf("number answer = %#v", dataset.Questions[1].GoldAnswers)
	}
	if stats.SkippedNullAnswer != 1 || stats.DroppedEvidence != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}

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
	dataset, stats, err := convertLongMemEval(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(dataset.Conversations) != 2 || stats.Conversations != 2 {
		t.Fatalf("conversations = %d, stats = %+v", len(dataset.Conversations), stats)
	}
	first := dataset.Conversations[0]
	if len(first.Turns) != 3 {
		t.Fatalf("first turns = %d", len(first.Turns))
	}
	if got := first.Turns[0].Content; !strings.Contains(got, "[2023/04/10 (Mon) 14:47] user: early evidence turn") {
		t.Fatalf("sessions not sorted chronologically: %q", got)
	}
	if first.Turns[1].Role != "assistant" {
		t.Fatalf("middle role = %q", first.Turns[1].Role)
	}
	question := dataset.Questions[0]
	if question.AskedAt != "2023/04/10 (Mon) 23:07" {
		t.Fatalf("asked_at = %q", question.AskedAt)
	}
	if len(question.EvidenceIDs) != 1 || question.EvidenceIDs[0] != "gpt4_abc:answer_x_3:t0" {
		t.Fatalf("evidence = %#v", question.EvidenceIDs)
	}
	second := dataset.Questions[1]
	if second.GoldAnswers[0] != "3" {
		t.Fatalf("number gold = %#v", second.GoldAnswers)
	}
	if !strings.Contains(second.ID, "_abs") {
		t.Fatalf("abs tag missing: %#v", second.Tags)
	}
}

func TestApplyConversationLimitKeepsQuestionsConsistent(t *testing.T) {
	dataset := Dataset{
		Conversations: []Conversation{{ID: "c1"}, {ID: "c2"}, {ID: "c3"}},
		Questions: []Question{
			{ID: "q1", ConversationID: "c1"},
			{ID: "q2", ConversationID: "c2"},
			{ID: "q3", ConversationID: "c3"},
		},
	}
	applyConversationLimit(&dataset, 2)
	if len(dataset.Conversations) != 2 || len(dataset.Questions) != 2 {
		t.Fatalf("limit = conversations %d questions %d", len(dataset.Conversations), len(dataset.Questions))
	}
	if dataset.Questions[0].ID != "q1" || dataset.Questions[1].ID != "q2" {
		t.Fatalf("questions = %#v", dataset.Questions)
	}
}

func TestDatasetRoundTrip(t *testing.T) {
	dataset := Dataset{
		Name: "roundtrip",
		Conversations: []Conversation{{
			ID: "c1",
			Turns: []Turn{
				{Role: "user", Content: "hello", EvidenceID: "c1:1", SessionID: "s1"},
			},
		}},
		Questions: []Question{{
			ID: "q1", ConversationID: "c1", Query: "who?", GoldAnswers: []string{"me"},
			Tags: []string{"x"}, EvidenceIDs: []string{"c1:1"}, AskedAt: "now",
		}},
	}
	var buffer bytes.Buffer
	if err := writeDataset(&buffer, dataset); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeDataset(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Conversations) != 1 || len(decoded.Questions) != 1 {
		t.Fatalf("decoded = %#v", decoded)
	}
	if decoded.Conversations[0].Turns[0].EvidenceID != "c1:1" ||
		decoded.Questions[0].AskedAt != "now" {
		t.Fatalf("decoded mismatch: %#v", decoded)
	}
}

func TestScoreEMF1(t *testing.T) {
	em, f1 := scoreEMF1("She lives in San Francisco now.", []string{"San Francisco"})
	if em != 1 {
		t.Fatalf("em = %v", em)
	}
	if f1 <= 0 {
		t.Fatalf("f1 = %v", f1)
	}
	em, f1 = scoreEMF1("unknown", []string{"San Francisco"})
	if em != 0 || f1 != 0 {
		t.Fatalf("miss: em=%v f1=%v", em, f1)
	}
}

func TestComputeKHit(t *testing.T) {
	evidence := map[uint64]bool{2: true, 5: true}
	items := []sdkmemory.ContextItem{
		{Kind: sdkmemory.ContextRawMessage, Sequence: 1},
		{Kind: sdkmemory.ContextRawMessage, Sequence: 2},
		{
			Kind: sdkmemory.ContextFact,
			Sources: []sdkmemory.SourceRef{{
				Kind: sdkmemory.SourceMessage, ID: "c/msg-00000000000000000005", Revision: "5",
			}},
		},
		{Kind: sdkmemory.ContextRawMessage, Sequence: 9},
	}
	hit := computeKHit(items, evidence)
	if !hit.Hit || !hit.Message || !hit.Fact {
		t.Fatalf("k_hit = %+v", hit)
	}
}

func TestCLIRejectsMissingRunFlags(t *testing.T) {
	err := runCLI(context.Background(), []string{"run"})
	if err == nil || !strings.Contains(err.Error(), "--dataset") {
		t.Fatalf("err = %v", err)
	}
}
