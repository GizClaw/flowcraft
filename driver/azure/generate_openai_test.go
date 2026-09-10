package azure

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// azureEntry declares one deployment for the compiler tests: text in, text
// out, image input when the test needs vision.
func azureEntry(inputs []message.PartKind) catalogEntry {
	outputs := []message.PartKind{message.PartText}
	return entryFor(ModelSpec{
		Name: "gpt-test",
		Kind: "generate",
		Capabilities: &inference.CapabilitiesPatch{
			Inputs:  &inputs,
			Outputs: &outputs,
		},
	})
}

func azureModel() inference.ModelRef {
	return inference.ModelRef{
		ID:      inference.ModelID{Provider: "azure", Name: "gpt-test"},
		Profile: "default",
	}
}

func azureWire(t *testing.T, entry catalogEntry, request inference.GenerateRequest) generateWire {
	t.Helper()
	compiled, err := compileGenerate("gpt-test", entry)(
		context.Background(),
		azureModel(),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compileGenerate: %v", err)
	}
	return compiled.Wire
}

func azureTextRequest(text string) inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{
					Parts: []message.Part{message.TextPart{Text: text}},
				},
				Intent: inference.Intent{Text: &inference.TextIntent{}},
			},
		},
	}
}

// Assistant context must ride an output-message item: the Responses API
// accepts output_text and refusal under the assistant role only, and an
// input_text part under that role fails the whole request with 400
// invalid_value. See issue #524.
func TestWireToParamsAssistantContextUsesOutputText(t *testing.T) {
	request := azureTextRequest("current")
	request.Context = []message.Message{
		{
			Role: message.RoleUser,
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "prior"},
			}},
		},
		{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "answer"},
			}},
		},
	}
	entry := azureEntry([]message.PartKind{message.PartText})
	items := wireToParams(azureWire(t, entry, request)).Input.OfInputItemList
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 (user, assistant, current)", len(items))
	}
	assistant := items[1]
	if assistant.OfOutputMessage == nil {
		t.Fatalf("assistant item = %+v, want an output-message item", assistant)
	}
	content := assistant.OfOutputMessage.Content
	if len(content) != 1 || content[0].OfOutputText == nil ||
		content[0].OfOutputText.Text != "answer" {
		t.Fatalf("assistant content = %+v, want one output_text part", content)
	}
	raw, err := json.Marshal(assistant)
	if err != nil {
		t.Fatalf("marshal assistant item: %v", err)
	}
	if !strings.Contains(string(raw), `"type":"output_text"`) {
		t.Fatalf("assistant item = %s, want output_text content", raw)
	}
	for _, index := range []int{0, 2} {
		other := items[index]
		if other.OfMessage == nil {
			t.Fatalf("item[%d] = %+v, want an easy-message item", index, other)
		}
		raw, err := json.Marshal(other)
		if err != nil {
			t.Fatalf("marshal item[%d]: %v", index, err)
		}
		if !strings.Contains(string(raw), `"type":"input_text"`) {
			t.Fatalf("item[%d] = %s, want input_text content", index, raw)
		}
	}
}

// The assistant output-message shape carries output_text only, so a media
// part on an assistant turn has no wire form: the compiler fails it locally
// instead of emitting a body the provider refuses.
func TestCompileRejectsAssistantContextImage(t *testing.T) {
	source, err := media.NewImageURL("https://example.com/cat.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	request := azureTextRequest("current")
	request.Context = []message.Message{
		{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "answer"},
				message.ImagePart{Source: source},
			}},
		},
	}
	entry := azureEntry([]message.PartKind{message.PartText, message.PartImage})
	_, err = compileGenerate("gpt-test", entry)(
		context.Background(),
		azureModel(),
		request,
		inference.GenerateExecutionUnary,
	)
	if err == nil {
		t.Fatal("compiler accepted an image in an assistant turn")
	}
	var inferenceErr *inference.Error
	if !errors.As(err, &inferenceErr) ||
		inferenceErr.Field != inference.FieldGenerateContextImage {
		t.Fatalf("compile error = %+v, want rejection of %q",
			err, inference.FieldGenerateContextImage)
	}
}
