package anthropic

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	anthropicgo "github.com/anthropics/anthropic-sdk-go"
)

// declaredEntry builds a one-model declared catalog carrying the given
// capability leaves.
func declaredEntry(t *testing.T, capabilities string) catalogEntry {
	t.Helper()
	spec, err := decodeSpec(context.Background(), []byte(
		`{"catalog":"declared","models":[{"name":"m","capabilities":`+
			capabilities+`}]}`,
	))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	models, err := mergedCatalog(spec)
	if err != nil {
		t.Fatalf("mergedCatalog: %v", err)
	}
	return models["m"]
}

func toolResultRequest(t *testing.T, parts ...message.Part) inference.GenerateRequest {
	t.Helper()
	request := conformanceTextRequest()
	request.Context = append(request.Context, message.Message{
		Role: message.RoleTool,
		Content: message.Content{Parts: []message.Part{
			message.ToolResultPart{Result: message.ToolResult{
				CallID:  "call_1",
				Content: message.Content{Parts: parts},
			}},
		}},
	})
	return request
}

func toolResultImage(t *testing.T) message.Part {
	t.Helper()
	source, err := media.NewImageBytes([]byte("png-bytes"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	return message.ImagePart{Source: source}
}

func toolResultVideo(t *testing.T) message.Part {
	t.Helper()
	source, err := media.NewVideoBytes([]byte("clip-bytes"), "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoBytes: %v", err)
	}
	return message.VideoPart{Source: source}
}

// TestToolResultCarriesMultimodalBlocks: the Messages tool_result content
// list carries text and image parts, so a vision model sees the screenshot a
// tool produced instead of a flattened string.
func TestToolResultCarriesMultimodalBlocks(t *testing.T) {
	entry := declaredEntry(t,
		`{"inputs":["text","image","tool_call","tool_result"],"outputs":["text"]}`)
	request := toolResultRequest(t,
		message.TextPart{Text: "before"},
		toolResultImage(t),
		message.TextPart{Text: "after"},
	)
	compiled, err := compileGenerate("m", entry)(
		context.Background(),
		conformanceModel("m"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if compiled.Report.Dropped(inference.FieldGenerateContextToolResult) {
		t.Fatalf("multimodal result must compile native: %+v", compiled.Report.Decisions)
	}
	block := findToolResult(t, compiled.Wire)
	if len(block.Content) != 3 ||
		block.Content[0].OfText == nil ||
		block.Content[1].OfImage == nil ||
		block.Content[2].OfText == nil {
		t.Fatalf("result blocks = %+v", block.Content)
	}
}

// TestToolResultOmittedPartKeepsPosition: a part the model cannot consume is
// replaced where it stood and reported on the ledger, never dropped silently.
func TestToolResultOmittedPartKeepsPosition(t *testing.T) {
	entry := declaredEntry(t,
		`{"inputs":["text","tool_call","tool_result"],"outputs":["text"]}`)
	request := toolResultRequest(t,
		message.TextPart{Text: "before"},
		toolResultImage(t),
		message.TextPart{Text: "after"},
	)
	compiled, err := compileGenerate("m", entry)(
		context.Background(),
		conformanceModel("m"),
		request,
		inference.GenerateExecutionUnary,
	)
	// A tool result degrades rather than failing the turn: the caller cannot
	// control what a tool returns.
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !compiled.Report.Dropped(inference.FieldGenerateContextToolResult) {
		t.Fatalf("decisions = %+v", compiled.Report.Decisions)
	}
	block := findToolResult(t, compiled.Wire)
	if len(block.Content) != 3 ||
		block.Content[1].OfText == nil ||
		block.Content[1].OfText.Text != "[omitted tool output: image (model does not accept image input)]" {
		t.Fatalf("result blocks = %+v", block.Content)
	}
}

// TestToolResultTextKeepsStringForm pins the compatible default: a single
// text part still rides the string form every endpoint accepts.
func TestToolResultTextKeepsStringForm(t *testing.T) {
	entry := declaredEntry(t,
		`{"inputs":["text","tool_call","tool_result"],"outputs":["text"]}`)
	request := toolResultRequest(t, message.TextPart{Text: "found"})
	compiled, err := compileGenerate("m", entry)(
		context.Background(),
		conformanceModel("m"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	block := findToolResult(t, compiled.Wire)
	if len(block.Content) != 1 ||
		block.Content[0].OfText == nil ||
		block.Content[0].OfText.Text != "found" {
		t.Fatalf("tool result = %+v", block.Content)
	}
}

func findToolResult(t *testing.T, params anthropicgo.MessageNewParams) anthropicgo.ToolResultBlockParam {
	t.Helper()
	for _, turn := range params.Messages {
		for _, block := range turn.Content {
			if block.OfToolResult != nil {
				return *block.OfToolResult
			}
		}
	}
	t.Fatalf("no tool_result block in %+v", params.Messages)
	return anthropicgo.ToolResultBlockParam{}
}
