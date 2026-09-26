package chat

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

func TestFactExtractorStableIDProvenancePromptAndClone(t *testing.T) {
	fake := &inferencetest.GenerateFake{Respond: jsonResponse(`{"facts":[{"text":"  Alice   likes tea ","entities":[],"event_time":""},{"text":"\t","entities":[],"event_time":""}]}`)}
	runtime := fake.Assembly(t)
	model := inferencetest.DefaultFakeModel
	extractor, err := NewFactExtractor(runtime, &model)
	if err != nil {
		t.Fatal(err)
	}
	input := rawMessageArtifact()
	first, err := extractor.Derive(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Content.Text() != "Alice likes tea" ||
		!reflect.DeepEqual(first[0].Sources, input.Sources) {
		t.Fatalf("facts = %#v", first)
	}
	reordered := input.Clone()
	reordered.Sources[0], reordered.Sources[1] = reordered.Sources[1], reordered.Sources[0]
	second, err := extractor.Derive(context.Background(), reordered)
	if err != nil || len(second) != 1 || second[0].ID != first[0].ID {
		t.Fatalf("stable retry = %#v, %v", second, err)
	}
	request := fake.LastRequest()
	if request.Input.Content.Intent.Text == nil ||
		request.Input.Content.Intent.Text.Response == nil ||
		request.Input.Content.Intent.Text.Response.Kind != inference.ResponseJSONSchema ||
		!strings.Contains(request.Input.Content.Text(), "Remember that I like tea") {
		t.Fatalf("generate request = %#v", request)
	}
	if len(request.Context) != 1 ||
		request.Context[0].Role != coremessage.RoleSystem ||
		!strings.Contains(request.Context[0].Content.Text(), "long-term memory extractor") {
		t.Fatalf("instructions must live in the system message: %#v", request.Context)
	}
	// The prompt carries the architecture rules the downstream consumers rely
	// on: multi-speaker attribution, absolute temporal grounding, composite
	// facts for multi-hop, inference evidence, and atomic entities.
	for _, rule := range []string{"MULTI-SPEAKER", "TEMPORAL GROUNDING", "COMPOSITE FACTS", "INFERENCE EVIDENCE", "ENTITIES"} {
		if !strings.Contains(request.Context[0].Content.Text(), rule) {
			t.Fatalf("system prompt is missing the %s rule", rule)
		}
	}
	if strings.Contains(request.Input.Content.Text(), "long-term memory extractor") {
		t.Fatalf("instructions leaked into the user message: %q", request.Input.Content.Text())
	}
	input.Sources[0].ID = "input mutation"
	input.Metadata["key"] = "input mutation"
	first[0].Sources[0].ID = "output mutation"
	first[0].Metadata["key"] = "output mutation"
	third, err := extractor.Derive(context.Background(), rawMessageArtifact())
	if err != nil || third[0].Sources[0].ID != "m2" || third[0].Metadata["key"] != "value" {
		t.Fatalf("output aliases input: %#v, %v", third, err)
	}
}

func TestFactExtractorMalformedEmptyAndWrongKind(t *testing.T) {
	model := inferencetest.DefaultFakeModel
	malformed := (&inferencetest.GenerateFake{Respond: jsonResponse(`{"facts":`)}).Assembly(t)
	extractor, _ := NewFactExtractor(malformed, &model)
	if _, err := extractor.Derive(context.Background(), rawMessageArtifact()); err == nil {
		t.Fatal("malformed JSON accepted")
	}
	emptyRuntime := (&inferencetest.GenerateFake{Respond: jsonResponse(`{"facts":[{"text":"  ","entities":[],"event_time":""}]}`)}).Assembly(t)
	extractor, _ = NewFactExtractor(emptyRuntime, &model)
	got, err := extractor.Derive(context.Background(), rawMessageArtifact())
	if err != nil || !reflect.DeepEqual(got, []component.Artifact{}) {
		t.Fatalf("empty facts = %#v, %v", got, err)
	}
	input := rawMessageArtifact()
	input.Kind = "document"
	if _, err := extractor.Derive(context.Background(), input); err == nil {
		t.Fatal("wrong input kind accepted")
	}
}

func TestFactExtractorPropagatesProviderFailure(t *testing.T) {
	runtime, model := failingRuntime(t)
	extractor, err := NewFactExtractor(runtime, &model)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := extractor.Derive(context.Background(), rawMessageArtifact()); err == nil ||
		!inference.IsKind(err, inference.ProviderFailure) {
		t.Fatalf("provider error = %v", err)
	}
}

func TestFactExtractorConstructorValidation(t *testing.T) {
	model := inferencetest.DefaultFakeModel
	if _, err := NewFactExtractor(nil, &model); err == nil {
		t.Fatal("nil runtime accepted")
	}
	runtime := (&inferencetest.GenerateFake{}).Assembly(t)
	if _, err := NewFactExtractor(runtime, nil); err == nil {
		t.Fatal("nil model accepted")
	}
}

func TestDecodeFactsLooseShapes(t *testing.T) {
	tests := []struct {
		name string
		data string
		want int
	}{
		{name: "facts array", data: `{"facts":[{"text":"a"}]}`, want: 1},
		{name: "singular fact object", data: `{"fact":{"text":"a"}}`, want: 1},
		{name: "singular fact array", data: `{"fact":[{"text":"a"},{"text":"b"}]}`, want: 2},
		{name: "result array", data: `{"result":[{"text":"a"}]}`, want: 1},
		{name: "empty facts array", data: `{"facts":[]}`, want: 0},
		{name: "facts as JSON string", data: `{"facts":"[{\"text\":\"a\"},{\"text\":\"b\"}]"}`, want: 2},
		{name: "top-level array", data: `[{"text":"a"}]`, want: 1},
		{name: "no facts", data: `{"foo":1}`, want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			batch, err := decodeFacts([]byte(test.data), false)
			if err != nil {
				if test.want == 0 && !strings.Contains(err.Error(), "no facts array") {
					t.Fatalf("decodeFacts(%s) unexpected error: %v", test.data, err)
				}
				if test.want > 0 {
					t.Fatalf("decodeFacts(%s) error: %v", test.data, err)
				}
				return
			}
			if len(batch.Facts) != test.want {
				t.Fatalf("decodeFacts(%s) = %#v, %v; want %d facts", test.data, batch, err, test.want)
			}
		})
	}
}

func rawMessageArtifact() component.Artifact {
	return component.Artifact{
		Kind: KindRawMessage, ID: "message",
		Content: coremessage.Content{Parts: []coremessage.Part{
			coremessage.TextPart{Text: "Remember that I like tea"},
		}},
		Sources: []corememory.SourceRef{
			{Kind: corememory.SourceMessage, ID: "m2", Revision: "2"},
			{Kind: corememory.SourceMessage, ID: "m1", Revision: "1"},
		},
		Metadata: corememory.Metadata{"key": "value"},
	}
}

func jsonResponse(value string) func(inference.GenerateRequest) inference.GenerateResponse {
	return func(inference.GenerateRequest) inference.GenerateResponse {
		return inference.GenerateResponse{
			Message: coremessage.Message{
				Role: coremessage.RoleAssistant,
				Content: coremessage.Content{Parts: []coremessage.Part{
					coremessage.TextPart{Text: value},
				}},
			},
			FinishReason: inference.FinishCompleted,
		}
	}
}

func failingRuntime(t *testing.T) (*inference.Assembly, model.ModelRef) {
	t.Helper()
	ref := model.ModelRef{
		ID: model.ModelID{Provider: "failure", Name: "model"}, Profile: "default",
	}
	driver, err := inference.BindGenerate(
		func(_ context.Context, _ model.ModelRef, request inference.GenerateRequest, shape inference.GenerateExecutionShape) (inference.Compiled[string], error) {
			return inference.Compiled[string]{
				Wire:   "wire",
				Report: inferencetest.NativeReport(model.OperationGenerate, request.ActiveFieldsFor(shape)...),
			}, nil
		},
		func(context.Context, string) (string, error) {
			return "", errors.New("provider unavailable")
		},
		func(context.Context, string) (inference.GenerateResponse, error) {
			return inference.GenerateResponse{}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime := newTestRuntime(t, []inference.ProviderDefinition{{
		ID: ref.ID.Provider,
		Profiles: []inference.ProfileDefinition{{
			ID: ref.Profile, Operations: []model.Operation{model.OperationGenerate},
		}},
		Models: []inference.ModelImplementation{{
			Descriptor: model.ModelDescriptor{ID: ref.ID},
			Openers: inference.Openers{
				Generate: func(context.Context, model.ModelRef) (inference.GenerateOperations, error) {
					return inference.GenerateOperations{Unary: driver}, nil
				},
			},
		}},
	}})
	return runtime, ref
}

// TestRichPromptDocumentsRichSchemaFields pins the review's schema/prompt
// mismatch: StrategyRich requires "predicate" and "temporal_detail" in the
// response schema, so the prompt for that strategy has to describe them. The
// simple prompt must stay free of them.
func TestRichPromptDocumentsRichSchemaFields(t *testing.T) {
	for _, field := range []string{"predicate", "temporal_detail"} {
		if !strings.Contains(factSystemRich, `"`+field+`"`) {
			t.Fatalf("rich prompt does not document %q", field)
		}
		if strings.Contains(factSystem, `"`+field+`"`) {
			t.Fatalf("simple prompt documents rich-only field %q", field)
		}
	}
	// The example response has to carry the rich keys with values, not just the
	// field names the loop above checked.
	if !strings.Contains(factSystemRich, `"predicate":"joined"`) ||
		!strings.Contains(factSystemRich, `"temporal_detail":"On 12 March 2023"`) {
		t.Fatal("rich prompt example does not carry the rich keys")
	}
	if got := factSystemFor(StrategyRich); got != factSystemRich {
		t.Fatal("the rich strategy does not use the rich prompt")
	}
	if got := factSystemFor(StrategySimple); got != factSystem {
		t.Fatal("the simple strategy does not use the simple prompt")
	}
}
