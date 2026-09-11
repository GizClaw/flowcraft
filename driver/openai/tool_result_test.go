package openai

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

const toolResultImageURL = "https://example.com/tool-output.png"

// toolResultRequest builds a context turn whose tool result carries content.
func toolResultRequest(t *testing.T, content message.Content) inference.GenerateRequest {
	t.Helper()
	request := simpleTextRequest("current")
	request.Context = []message.Message{{
		Role: message.RoleTool,
		Content: message.Content{Parts: []message.Part{
			message.ToolResultPart{Result: message.ToolResult{
				CallID:  "call_1",
				Content: content,
			}},
		}},
	}}
	return request
}

// toolResultImageParts builds the text/image/text content a multimodal tool
// returns; the image rides a URL so the transport never needs to materialize
// bytes.
func toolResultImageParts(t *testing.T) message.Content {
	t.Helper()
	source, err := media.NewImageURL(toolResultImageURL, "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	return message.Content{Parts: []message.Part{
		message.TextPart{Text: "before"},
		message.ImagePart{Source: source},
		message.TextPart{Text: "after"},
	}}
}

func compileToolResult(t *testing.T, entry catalogEntry, content message.Content) inference.Compiled[generateWire] {
	t.Helper()
	compiled, err := compileGenerate("gpt-5.6-sol", entry)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		toolResultRequest(t, content),
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	return compiled
}

// TestToolResultMultimodalCarriesImage locks the Responses shape: a vision
// model receives the tool's image in the function_call_output content list,
// in the order the tool produced it.
func TestToolResultMultimodalCarriesImage(t *testing.T) {
	compiled := compileToolResult(t, catalog["gpt-5.6-sol"], toolResultImageParts(t))
	if compiled.Report.Dropped(inference.FieldGenerateContextToolResult) {
		t.Fatalf("multimodal tool result must compile native: %+v", compiled.Report.Decisions)
	}
	params := wireToParams(compiled.Wire)
	output := params.Input.OfInputItemList[0].OfFunctionCallOutput.Output
	list := output.OfResponseFunctionCallOutputItemArray
	if len(list) != 3 {
		t.Fatalf("tool output items = %d, want 3", len(list))
	}
	if list[0].OfInputText == nil || list[0].OfInputText.Text != "before" {
		t.Fatalf("item 0 = %+v", list[0])
	}
	if list[1].OfInputImage == nil ||
		list[1].OfInputImage.ImageURL.Value != toolResultImageURL {
		t.Fatalf("item 1 = %+v", list[1])
	}
	if list[2].OfInputText == nil || list[2].OfInputText.Text != "after" {
		t.Fatalf("item 2 = %+v", list[2])
	}
}

// TestToolResultOmittedPartKeepsPosition locks the downgrade shape: a part the
// model cannot consume is replaced where it stood, so the surrounding text
// keeps its meaning, and the loss is reported on the ledger.
func TestToolResultOmittedPartKeepsPosition(t *testing.T) {
	entry := catalog["gpt-5.6-sol"]
	entry.capabilities.Inputs = []message.PartKind{
		message.PartText,
		message.PartData,
		message.PartToolCall,
		message.PartToolResult,
	}
	compiled := compileToolResult(t, entry, toolResultImageParts(t))
	if !compiled.Report.Dropped(inference.FieldGenerateContextToolResult) {
		t.Fatal("an unconsumable tool result part must be reported as dropped")
	}
	items := compiled.Wire.items[0].output
	if len(items) != 3 {
		t.Fatalf("lowered parts = %d, want 3 (text, placeholder, text)", len(items))
	}
	if items[0].text != "before" || items[2].text != "after" {
		t.Fatalf("surrounding text lost its place: %+v", items)
	}
	want := "[omitted tool output: image (model does not accept image input)]"
	if items[1].kind != wireContentText || items[1].text != want {
		t.Fatalf("placeholder = %+v, want %q", items[1], want)
	}
	notes := compiled.Report.Components(inference.FieldGenerateContextToolResult)
	if len(notes) != 3 {
		t.Fatalf("components = %+v, want one note per part", notes)
	}
	if notes[0].Kind != message.PartText || notes[0].Disposition != inference.Native ||
		notes[1].Kind != message.PartImage || notes[1].Disposition != inference.Dropped ||
		notes[1].Index != 1 ||
		notes[2].Kind != message.PartText || notes[2].Disposition != inference.Native {
		t.Fatalf("components = %+v", notes)
	}
	if notes[1].Reason == "" {
		t.Fatal("dropped component must name its reason")
	}
}

// TestToolResultChatImagesAreReportedNotSilentlyDropped pins the Chat
// Completions contract: tool messages carry text only, so an image becomes an
// in-place placeholder with a ledger reason instead of vanishing.
func TestToolResultChatImagesAreReportedNotSilentlyDropped(t *testing.T) {
	entry := catalog["gpt-5.6-sol"]
	entry.dialect.api = apiChat
	content := message.Content{Parts: []message.Part{
		message.TextPart{Text: "before"},
		message.ImagePart{Source: mustImageSource(t)},
	}}
	compiled := compileToolResult(t, entry, content)
	if !compiled.Report.Dropped(inference.FieldGenerateContextToolResult) {
		t.Fatal("chat tool images must be reported as dropped")
	}
	params := wireToChatParams(compiled.Wire)
	var tool *string
	for _, item := range params.Messages {
		if item.OfTool != nil {
			text := item.OfTool.Content.OfString.Value
			tool = &text
		}
	}
	if tool == nil {
		t.Fatalf("no tool message in %+v", params.Messages)
	}
	want := "before[omitted tool output: image (chat tool messages carry text only)]"
	if *tool != want {
		t.Fatalf("tool message = %q, want %q", *tool, want)
	}
	notes := compiled.Report.Components(inference.FieldGenerateContextToolResult)
	if len(notes) != 2 ||
		notes[1].Kind != message.PartImage ||
		notes[1].Index != 1 ||
		notes[1].Disposition != inference.Dropped {
		t.Fatalf("components = %+v", notes)
	}
}

func mustImageSource(t *testing.T) media.ImageSource {
	t.Helper()
	source, err := media.NewImageURL(toolResultImageURL, "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	return source
}
