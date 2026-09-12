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
	item := compiled.Wire.GetInput().GetListValue().GetListValue()[0]
	message := item.GetEasyMessage()
	if message == nil || message.GetRole() != arkresponses.MessageRole_user {
		t.Fatalf("item = %+v, want user message", item)
	}
	audio := message.GetContent().GetListValue().GetListValue()[0].GetAudio()
	if audio == nil || audio.GetAudioUrl() != "https://example.com/audio.mp3" {
		t.Fatalf("content = %+v, want audio url content", audio)
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
	audio := compiled.Wire.GetInput().GetListValue().GetListValue()[0].
		GetEasyMessage().GetContent().GetListValue().GetListValue()[0].GetAudio()
	if audio == nil {
		t.Fatal("content has no audio part")
	}
	if want := "data:audio/mpeg;base64,AAEC"; audio.GetAudioUrl() != want {
		t.Fatalf("audio uri = %q, want %q", audio.GetAudioUrl(), want)
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
	request := &arkresponses.ResponsesRequest{}
	appendInputItem(request, arkMessageItem("user", []*arkresponses.ContentItem{
		arkContentAudio("https://example.com/audio.mp3"),
	}))
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
