package inference

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

func TestEmbedActiveFieldsCoverEveryPartKindAndMultiPart(t *testing.T) {
	image, _ := media.NewImageURL("https://example.com/cat.png", "image/png")
	audio, _ := media.NewAudioBytes([]byte("audio"), "audio/mpeg")
	video, _ := media.NewVideoURL("https://example.com/video.mp4", "video/mp4")
	call, _ := message.NewCall("call-1", "search", map[string]any{"query": "cat"})
	request := EmbedRequest{Items: []EmbedItem{{Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hello"},
		message.ImagePart{Source: image},
		message.AudioPart{Source: audio},
		message.VideoPart{Source: video},
		message.FilePart{URI: "s3://bucket/document.pdf"},
		message.DataPart{Value: json.RawMessage(`{"answer":42}`)},
		message.ToolCallPart{Call: call},
		message.ToolResultPart{Result: message.Result{CallID: "call-1", Content: "found"}}},
	}}}}
	if err := request.Validate(); err != nil {
		t.Fatalf("EmbedRequest.Validate: %v", err)
	}

	got := make(map[FieldID]bool)
	for _, field := range request.ActiveFields() {
		got[field] = true
	}
	for _, field := range []FieldID{
		FieldEmbedItems,
		FieldEmbedItemText,
		FieldEmbedItemImage,
		FieldEmbedItemAudio,
		FieldEmbedItemVideo,
		FieldEmbedItemFile,
		FieldEmbedItemData,
		FieldEmbedItemToolCall,
		FieldEmbedItemToolResult,
		FieldEmbedItemMultiPart,
	} {
		if !got[field] {
			t.Errorf("active fields omitted %q", field)
		}
	}
}

func TestEmbedCompilerDecidesCanonicalPartSupport(t *testing.T) {
	type wire struct{}
	audio, err := media.NewAudioBytes([]byte("audio"), "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	compileCalls := 0
	driver, err := BindEmbed(
		func(_ context.Context, _ ModelRef, request EmbedRequest) (Compiled[wire], error) {
			compileCalls++
			if _, ok := request.Items[0].Content.Parts[0].(message.AudioPart); !ok {
				t.Fatalf("compiler received part %T, want message.AudioPart", request.Items[0].Content.Parts[0])
			}
			return Compiled[wire]{Report: CompileReport{
				Operation: OperationEmbed,
				Decisions: []Decision{
					{Field: FieldEmbedItems, Disposition: Native},
					{Field: FieldEmbedItemAudio, Disposition: Native},
				},
			}}, nil
		},
		func(context.Context, wire) (struct{}, error) {
			t.Fatal("Explain must not invoke transport")
			return struct{}{}, nil
		},
		func(context.Context, struct{}) (EmbedResponse, error) {
			return EmbedResponse{}, nil
		},
	)
	if err != nil {
		t.Fatalf("BindEmbed: %v", err)
	}
	explanation, err := driver.Explain(
		context.Background(),
		ModelRef{ID: ModelID{Provider: "fake", Name: "audio-embed"}},
		EmbedRequest{Items: []EmbedItem{{Content: message.Content{Parts: []message.Part{message.AudioPart{Source: audio}}}}}},
	)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if compileCalls != 1 || len(explanation.Decisions) != 2 {
		t.Fatalf("compile calls=%d explanation=%+v", compileCalls, explanation)
	}
}
