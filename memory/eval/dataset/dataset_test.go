package dataset

import (
	"bytes"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

func TestDatasetRoundTrip(t *testing.T) {
	dataset := Dataset{
		Name: "roundtrip",
		Conversations: []Conversation{{
			ID: "c1",
			Turns: []Turn{
				{
					Message: message.Message{
						Role:    message.RoleUser,
						Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hello"}}},
					},
					EvidenceID: "c1:1",
					SessionID:  "s1",
				},
			},
		}},
		Questions: []Question{{
			ID: "q1", ConversationID: "c1", Query: "who?", GoldAnswers: []string{"me"},
			Tags: []string{"x"}, EvidenceIDs: []string{"c1:1"}, AskedAt: "now",
		}},
	}
	var buffer bytes.Buffer
	if err := Write(&buffer, dataset); err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(&buffer)
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
	imageSource, err := media.NewImageURL("https://example.invalid/x.png", "")
	if err != nil {
		t.Fatal(err)
	}
	multimodal := Dataset{
		Conversations: []Conversation{{
			ID: "c2",
			Turns: []Turn{{
				Message: message.Message{
					Role: message.RoleUser,
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: "look"},
						message.ImagePart{Source: imageSource},
						TurnDataPart("Caroline", "s1", "2023-05-08", "c2:1"),
					}},
				},
				EvidenceID: "c2:1",
				SessionID:  "s1",
			}},
		}},
	}
	buffer.Reset()
	if err := Write(&buffer, multimodal); err != nil {
		t.Fatal(err)
	}
	decodedMultimodal, err := Decode(&buffer)
	if err != nil {
		t.Fatal(err)
	}
	turn := decodedMultimodal.Conversations[0].Turns[0]
	if len(turn.Content.Parts) != 3 {
		t.Fatalf("multimodal parts = %d, want 3: %#v", len(turn.Content.Parts), turn.Content.Parts)
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
	ApplyConversationLimit(&dataset, 2)
	if len(dataset.Conversations) != 2 || len(dataset.Questions) != 2 {
		t.Fatalf("limit = conversations %d questions %d", len(dataset.Conversations), len(dataset.Questions))
	}
	if dataset.Questions[0].ID != "q1" || dataset.Questions[1].ID != "q2" {
		t.Fatalf("questions = %#v", dataset.Questions)
	}
}
