package chat

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

func TestFactExtractorStableIDProvenancePromptAndClone(t *testing.T) {
	fake := &inferencetest.GenerateFake{Respond: jsonResponse(`{"facts":[{"text":"  Alice   likes tea "},{"text":"\t"}]}`)}
	runtime := fake.Runtime(t)
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
	malformed := (&inferencetest.GenerateFake{Respond: jsonResponse(`{"facts":`)}).Runtime(t)
	extractor, _ := NewFactExtractor(malformed, &model)
	if _, err := extractor.Derive(context.Background(), rawMessageArtifact()); err == nil {
		t.Fatal("malformed JSON accepted")
	}
	emptyRuntime := (&inferencetest.GenerateFake{Respond: jsonResponse(`{"facts":[{"text":"  "}]}`)}).Runtime(t)
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
	runtime := (&inferencetest.GenerateFake{}).Runtime(t)
	if _, err := NewFactExtractor(runtime, nil); err == nil {
		t.Fatal("nil model accepted")
	}
}

func rawMessageArtifact() component.Artifact {
	return component.Artifact{
		Kind: KindRawMessage, ID: "message",
		Content: sdkmessage.Content{Parts: []sdkmessage.Part{
			sdkmessage.TextPart{Text: "Remember that I like tea"},
		}},
		Sources: []sdkmemory.SourceRef{
			{Kind: sdkmemory.SourceMessage, ID: "m2", Revision: "2"},
			{Kind: sdkmemory.SourceMessage, ID: "m1", Revision: "1"},
		},
		Metadata: sdkmemory.Metadata{"key": "value"},
	}
}

func jsonResponse(value string) func(inference.GenerateRequest) inference.GenerateResponse {
	return func(inference.GenerateRequest) inference.GenerateResponse {
		return inference.GenerateResponse{
			Message: sdkmessage.Message{
				Role: sdkmessage.RoleAssistant,
				Content: sdkmessage.Content{Parts: []sdkmessage.Part{
					sdkmessage.TextPart{Text: value},
				}},
			},
			FinishReason: inference.FinishCompleted,
		}
	}
}

func failingRuntime(t *testing.T) (*inference.Runtime, inference.ModelRef) {
	t.Helper()
	model := inference.ModelRef{
		ID: inference.ModelID{Provider: "failure", Name: "model"}, Profile: "default",
	}
	driver, err := inference.BindGenerate(
		func(_ context.Context, _ inference.ModelRef, request inference.GenerateRequest, shape inference.GenerateExecutionShape) (inference.Compiled[string], error) {
			return inference.Compiled[string]{
				Wire:   "wire",
				Report: inferencetest.NativeReport(inference.OperationGenerate, request.ActiveFieldsFor(shape)...),
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
	runtime, err := inference.NewRuntime([]inference.ProviderDefinition{{
		ID: model.ID.Provider,
		Profiles: []inference.ProfileDefinition{{
			ID: model.Profile, Operations: []inference.Operation{inference.OperationGenerate},
		}},
		Models: []inference.ModelImplementation{{
			Descriptor: inference.ModelDescriptor{ID: model.ID},
			Openers: inference.Openers{
				Generate: func(context.Context, inference.ModelRef) (inference.GenerateOperations, error) {
					return inference.GenerateOperations{Unary: driver}, nil
				},
			},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return runtime, model
}
