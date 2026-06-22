package source

import (
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestMessageFromTurnPreservesRawSourceMessageParts(t *testing.T) {
	session := dataset.Session{Index: 1, DateTime: "2024-01-01 09:00"}
	turn := dataset.Turn{
		DiaID:       "d2",
		Speaker:     "Ben",
		Text:        "What is in the image?",
		ImgURL:      "https://example.test/mug.png",
		BlipCaption: "a red mug on a table",
		Query:       "What is shown?",
	}

	msg := MessageFromTurn(session, turn)
	if msg.Role != model.RoleUser {
		t.Fatalf("turn role = %q, want user for transcript source message", msg.Role)
	}
	if got := msg.Metadata["source_kind"]; got != "locomo_turn" {
		t.Fatalf("source_kind metadata = %v, want locomo_turn", got)
	}
	if got := msg.Metadata["speaker"]; got != "Ben" {
		t.Fatalf("speaker metadata = %v, want Ben", got)
	}
	if len(msg.Parts) != 3 {
		t.Fatalf("message parts = %+v, want data, text, and image parts", msg.Parts)
	}
	if msg.Parts[0].Type != model.PartData || msg.Parts[0].Data == nil {
		t.Fatalf("first message part = %+v, want data part", msg.Parts[0])
	}
	if msg.Parts[0].Data.MimeType != HistoryMessageMimeType {
		t.Fatalf("data mime type = %q, want %q", msg.Parts[0].Data.MimeType, HistoryMessageMimeType)
	}
	data := msg.Parts[0].Data.Value
	if got := data["speaker_name"]; got != "Ben" {
		t.Fatalf("data speaker_name = %v, want Ben", got)
	}
	if got := data["session_datetime"]; got != "2024-01-01 09:00" {
		t.Fatalf("data session_datetime = %v, want session time", got)
	}
	if got := data["image_query"]; got != "What is shown?" {
		t.Fatalf("data image_query = %v, want query", got)
	}
	if msg.Parts[2].Type != model.PartImage || msg.Parts[2].Image.URL == "" {
		t.Fatalf("third message part = %+v, want image part", msg.Parts[2])
	}
	text := msg.Parts[1].Text
	if !strings.Contains(text, "What is in the image?") || !strings.Contains(text, "Image caption: a red mug on a table") {
		t.Fatalf("message text = %q, want utterance and extractable image caption", text)
	}
	for _, label := range []string{"date:", "speaker:", "Session 1", "image_url:", "Image search query:"} {
		if strings.Contains(text, label) {
			t.Fatalf("message text = %q, should not contain metadata label %q", text, label)
		}
	}
}

func TestMessageFromTurnParsesLocomoSessionDateTime(t *testing.T) {
	session := dataset.Session{
		Index:    1,
		DateTime: "8:56 pm on 20 July, 2023",
	}
	turn := dataset.Turn{DiaID: "d1", Speaker: "Ada", Text: "Hi."}

	msg := MessageFromTurn(session, turn)
	if msg.CreatedAt.IsZero() {
		t.Fatal("CreatedAt is zero, want parsed LoCoMo session time")
	}
	if msg.CreatedAt.Year() != 2023 || msg.CreatedAt.Month() != time.July || msg.CreatedAt.Day() != 20 {
		t.Fatalf("CreatedAt date = %s, want 2023-07-20", msg.CreatedAt.Format(time.RFC3339))
	}
	if msg.CreatedAt.Hour() != 20 || msg.CreatedAt.Minute() != 56 {
		t.Fatalf("CreatedAt clock = %02d:%02d, want 20:56", msg.CreatedAt.Hour(), msg.CreatedAt.Minute())
	}
}
