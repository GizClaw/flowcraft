package bytedance

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
)

func audioRequest(source media.AudioSource) inference.GenerateRequest {
	return inference.GenerateRequest{Input: inference.GenerateInput{
		Role: inference.InputRoleUser,
		Content: inference.InputContent{
			Content: message.Content{Parts: []message.Part{message.AudioPart{Source: source}}},
			Intent:  inference.Intent{Text: &inference.TextIntent{}},
		},
	}}
}

func TestCompileGenerateAudioInputURL(t *testing.T) {
	source, err := media.NewAudioURL("https://example.com/audio.mp3", "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioURL: %v", err)
	}
	compiled, err := compileGenerate("doubao-seed-2-0-lite", catalog["doubao-seed-2-0-lite"])(
		context.Background(),
		conformanceModel("doubao-seed-2-0-lite"),
		audioRequest(source),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	item := compiled.Wire.items[0]
	if item.kind != wireItemMessage || item.role != "user" {
		t.Fatalf("item = %+v, want user message", item)
	}
	if len(item.content) != 1 ||
		item.content[0].kind != wireContentAudio ||
		item.content[0].uri != "https://example.com/audio.mp3" {
		t.Fatalf("content = %+v, want audio url content", item.content)
	}
}

func TestCompileGenerateAudioInputInline(t *testing.T) {
	source, err := media.NewAudioBytes([]byte{0x00, 0x01, 0x02}, "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	compiled, err := compileGenerate("doubao-seed-2-0-mini", catalog["doubao-seed-2-0-mini"])(
		context.Background(),
		conformanceModel("doubao-seed-2-0-mini"),
		audioRequest(source),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	content := compiled.Wire.items[0].content
	if len(content) != 1 || content[0].kind != wireContentAudio {
		t.Fatalf("content = %+v, want audio content", content)
	}
	if want := "data:audio/mpeg;base64,AAEC"; content[0].uri != want {
		t.Fatalf("audio uri = %q, want %q", content[0].uri, want)
	}
}

func TestCompileGenerateRejectsAudioWithoutCapability(t *testing.T) {
	source, err := media.NewAudioURL("https://example.com/audio.mp3", "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioURL: %v", err)
	}
	compiled, err := compileGenerate("doubao-seed-2-1-pro", catalog["doubao-seed-2-1-pro"])(
		context.Background(),
		conformanceModel("doubao-seed-2-1-pro"),
		audioRequest(source),
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("compileGenerate unexpectedly accepted audio input")
	}
	var compileErr *inference.Error
	if !errors.As(err, &compileErr) {
		t.Fatalf("error type = %T, want *inference.Error", err)
	}
	if compileErr.Kind != inference.UnsupportedFeature ||
		compileErr.Field != inference.FieldGenerateInputAudio {
		t.Fatalf("compile error = %+v, want input audio rejection", compileErr)
	}
	if !compiled.Report.Rejects(inference.FieldGenerateInputAudio) {
		t.Fatalf("report = %+v, want input audio rejected", compiled.Report)
	}
}

func TestWireToArkAudioContent(t *testing.T) {
	request := wireToArk(generateWire{items: []wireItem{{
		kind:    wireItemMessage,
		role:    "user",
		content: []wireContent{{kind: wireContentAudio, uri: "https://example.com/audio.mp3"}},
	}}})
	items := request.GetInput().GetListValue().GetListValue()
	if len(items) != 1 {
		t.Fatalf("input items = %d, want 1", len(items))
	}
	content := items[0].GetEasyMessage().GetContent().GetListValue().GetListValue()
	if len(content) != 1 {
		t.Fatalf("content items = %d, want 1", len(content))
	}
	audio := content[0].GetAudio()
	if audio == nil {
		t.Fatal("content item has no audio union")
	}
	if audio.GetType() != arkresponses.ContentItemType_input_audio {
		t.Fatalf("audio type = %v, want input_audio", audio.GetType())
	}
	if audio.GetAudioUrl() != "https://example.com/audio.mp3" {
		t.Fatalf("audio url = %q", audio.GetAudioUrl())
	}
}
