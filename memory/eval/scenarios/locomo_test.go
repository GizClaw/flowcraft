package scenarios

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
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
	converted, stats, err := (locomoScenario{}).Convert(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(converted.Conversations) != 1 {
		t.Fatalf("conversations = %d", len(converted.Conversations))
	}
	conversation := converted.Conversations[0]
	if conversation.ID != "conv-26" || len(conversation.Turns) != 4 {
		t.Fatalf("conversation = %#v", conversation)
	}
	if got := conversation.Turns[0].Message.Content.Text(); !strings.Contains(got, "[1:56 pm on 8 May, 2023] Caroline: Hello Mel!") {
		t.Fatalf("turn content = %q", got)
	}
	if conversation.Turns[1].Message.Role != message.RoleAssistant {
		t.Fatalf("speaker_b role = %q", conversation.Turns[1].Message.Role)
	}
	imageTurn := conversation.Turns[3]
	if got := imageTurn.Message.Content.Text(); !strings.Contains(got, "[shared image: transgender pride flag mural]") {
		t.Fatalf("image annotation = %q", got)
	}
	var hasImage, hasData bool
	for _, part := range imageTurn.Message.Content.Parts {
		switch part.Kind() {
		case message.PartImage:
			hasImage = true
		case message.PartData:
			hasData = true
		}
	}
	if !hasImage || !hasData {
		t.Fatalf("image turn parts missing image/data: %#v", imageTurn.Message.Content.Parts)
	}
	if len(converted.Questions) != 3 {
		t.Fatalf("questions = %d, want 3 (null-answer row skipped)", len(converted.Questions))
	}
	if got := converted.Questions[0].EvidenceIDs; len(got) != 1 || got[0] != "D1:3" {
		t.Fatalf("evidence[0] = %#v, want [D1:3] without prefix", got)
	}
	if got := converted.Questions[1].EvidenceIDs; len(got) != 2 || got[0] != "D1:3" || got[1] != "D1:4" {
		t.Fatalf("semicolon evidence = %#v", got)
	}
	if got := converted.Questions[2].EvidenceIDs; len(got) != 1 || got[0] != "D1:3" {
		t.Fatalf("invalid evidence not dropped: %#v", got)
	}
	if converted.Questions[1].GoldAnswers[0] != "2022" {
		t.Fatalf("number answer = %#v", converted.Questions[1].GoldAnswers)
	}
	if stats.SkippedNullAnswer != 1 || stats.DroppedEvidence != 2 {
		t.Fatalf("stats = %+v", stats)
	}
}
