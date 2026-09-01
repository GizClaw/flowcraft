package minimax

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
)

func renderMessageParams(t *testing.T, params anthropicgo.MessageNewParams) string {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return string(raw)
}

func TestM3VideoInputCompiles(t *testing.T) {
	compile := compileGenerate("MiniMax-M3", catalog["MiniMax-M3"])
	request := conformanceTextRequest()
	request.Input.Content.Parts = append(
		request.Input.Content.Parts,
		videoClipPart(t, "https://example.com/clip.mp4"),
	)
	compiled, err := compile(
		context.Background(),
		conformanceModel("MiniMax-M3"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	blocks := compiled.Wire.messages[0].blocks
	found := false
	for _, block := range blocks {
		if block.kind == wireBlockVideo {
			found = true
			if block.videoURL != "https://example.com/clip.mp4" {
				t.Fatalf("video block URL = %q", block.videoURL)
			}
		}
	}
	if !found {
		t.Fatalf("no video block in wire: %+v", blocks)
	}
}

func TestM2xVideoInputRejects(t *testing.T) {
	compile := compileGenerate("MiniMax-M2.7", catalog["MiniMax-M2.7"])
	request := conformanceTextRequest()
	request.Input.Content.Parts = append(
		request.Input.Content.Parts,
		videoClipPart(t, "https://example.com/clip.mp4"),
	)
	compiled, err := compile(
		context.Background(),
		conformanceModel("MiniMax-M2.7"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("video input on text-only M2.x unexpectedly accepted")
	}
	if !compiled.Report.Rejects(inference.FieldGenerateInputVideo) {
		t.Fatal("video input was not rejected on the input video field")
	}
}

func TestM3VideoBlockRendersRawUnion(t *testing.T) {
	compile := compileGenerate("MiniMax-M3", catalog["MiniMax-M3"])
	request := conformanceTextRequest()
	request.Input.Content.Parts = append(
		request.Input.Content.Parts,
		videoClipPart(t, "https://example.com/clip.mp4"),
	)
	compiled, err := compile(
		context.Background(),
		conformanceModel("MiniMax-M3"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	params := wireToParams(compiled.Wire)
	rendered := renderMessageParams(t, params)
	if !strings.Contains(rendered, `"type":"video"`) ||
		!strings.Contains(rendered, `"url":"https://example.com/clip.mp4"`) {
		t.Fatalf("video block not rendered as a raw video content block: %s", rendered)
	}
}

func TestM3InlineVideoBlockRendersBase64(t *testing.T) {
	compile := compileGenerate("MiniMax-M3", catalog["MiniMax-M3"])
	source, err := media.NewVideoBytes([]byte("clip-bytes"), "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoBytes: %v", err)
	}
	request := conformanceTextRequest()
	request.Input.Content.Parts = append(
		request.Input.Content.Parts,
		message.VideoPart{Source: source},
	)
	compiled, err := compile(
		context.Background(),
		conformanceModel("MiniMax-M3"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	params := wireToParams(compiled.Wire)
	rendered := renderMessageParams(t, params)
	if !strings.Contains(rendered, `"type":"base64"`) ||
		!strings.Contains(rendered, `"data":"Y2xpcC1ieXRlcw=="`) {
		t.Fatalf("inline video block not rendered as base64: %s", rendered)
	}
}

func TestM3VideoStreamSourceRejectsBeforeEncoding(t *testing.T) {
	pipe := media.NewPipe[string](1)
	source, err := media.NewVideoStream(pipe, "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoStream: %v", err)
	}
	compile := compileGenerate("MiniMax-M3", catalog["MiniMax-M3"])
	request := conformanceTextRequest()
	request.Input.Content.Parts = append(
		request.Input.Content.Parts,
		message.VideoPart{Source: source},
	)
	compiled, err := compile(
		context.Background(),
		conformanceModel("MiniMax-M3"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("stream video input unexpectedly compiled")
	}
	if !compiled.Report.Rejects(inference.FieldGenerateInputVideo) {
		t.Fatal("stream video input was not rejected on the input video field")
	}
}
