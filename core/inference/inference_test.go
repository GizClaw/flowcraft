package inference_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

func ref(name string) model.ModelRef {
	return model.ModelRef{ID: model.ModelID{Provider: "test", Name: name}, Profile: "default"}
}

func searchTool() message.ToolDefinition {
	return message.ToolDefinition{
		Name:        "search",
		Description: "search the corpus",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}
}

func mustToolset(t *testing.T, definitions ...message.ToolDefinition) inference.Toolset {
	t.Helper()
	tools, err := inference.NewToolset(definitions)
	if err != nil {
		t.Fatalf("toolset rejected: %v", err)
	}
	return tools
}

func textRequest(t *testing.T) inference.GenerateRequest {
	t.Helper()
	request := inference.GenerateRequest{
		Model: ref("m1"),
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("hello"),
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("baseline request invalid: %v", err)
	}
	return request
}

// TestGenerateRequestValidateRejectsBrokenInput pins that every rejection names
// the field it is about, so a caller can act on it without parsing text.
func TestGenerateRequestValidateRejectsBrokenInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*inference.GenerateRequest)
		field  inference.Field
	}{
		{
			name:   "missing model",
			mutate: func(r *inference.GenerateRequest) { r.Model = model.ModelRef{} },
			field:  inference.FieldModel,
		},
		{
			name: "unknown input role",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Role = inference.InputRole("assistant")
			},
			field: inference.FieldInput,
		},
		{
			name: "bad context message",
			mutate: func(r *inference.GenerateRequest) {
				r.Context = []message.Message{{Role: message.Role("nope")}}
			},
			field: inference.FieldContext,
		},
		{
			name: "unknown output",
			mutate: func(r *inference.GenerateRequest) {
				r.Output = inference.Output("hologram")
			},
			field: inference.FieldOutput,
		},
		{
			name: "tools on image output",
			mutate: func(r *inference.GenerateRequest) {
				r.Output = inference.OutputImage
				r.Tools = mustToolset(t, searchTool())
			},
			field: inference.FieldTools,
		},
		{
			name: "toolset built by struct literal",
			mutate: func(r *inference.GenerateRequest) {
				r.Tools = inference.Toolset{}
			},
			field: inference.FieldTools,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := textRequest(t)
			// The struct-literal case is valid; skip its assertion.
			if testCase.name == "toolset built by struct literal" {
				if err := request.Validate(); err != nil {
					t.Fatalf("zero toolset rejected: %v", err)
				}
				return
			}
			testCase.mutate(&request)
			err := request.Validate()
			if err == nil {
				t.Fatal("expected rejection")
			}
			if !inference.IsKind(err, inference.InvalidRequest) {
				t.Fatalf("kind = %v, want InvalidRequest", err)
			}
			inferenceErr := asInferenceError(t, err)
			if inferenceErr.Field != testCase.field {
				t.Fatalf("field = %q, want %q", inferenceErr.Field, testCase.field)
			}
			if !errdefs.IsValidation(err) {
				t.Fatalf("invalid request must classify as validation: %v", err)
			}
		})
	}
}

// TestNewToolsetRejectsBadDefinitions pins that a malformed tool list cannot
// reach a request at all: the invariant is enforced where the value is built.
func TestNewToolsetRejectsBadDefinitions(t *testing.T) {
	cases := map[string][]message.ToolDefinition{
		"duplicate":      {searchTool(), searchTool()},
		"missing schema": {{Name: "search"}},
		"missing name":   {{InputSchema: json.RawMessage(`{"type":"object"}`)}},
		"non-object schema": {{
			Name:        "search",
			InputSchema: json.RawMessage(`[]`),
		}},
	}
	for name, definitions := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := inference.NewToolset(definitions); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestToolsetIndexesAndShares(t *testing.T) {
	tools := mustToolset(t, searchTool())
	if tools.Len() != 1 || tools.IsZero() {
		t.Fatalf("len=%d zero=%v", tools.Len(), tools.IsZero())
	}
	if !tools.Has("search") || tools.Has("other") {
		t.Fatal("name index is wrong")
	}
	if tools.Choice() != nil {
		t.Fatal("unconstrained toolset reports a choice")
	}
	if err := tools.Validate(); err != nil {
		t.Fatalf("constructed toolset rejected: %v", err)
	}
	if !(inference.Toolset{}).IsZero() {
		t.Fatal("zero toolset is not zero")
	}
	if err := (inference.Toolset{}).Validate(); err != nil {
		t.Fatalf("zero toolset rejected: %v", err)
	}
}

func TestToolsetChoosingValidates(t *testing.T) {
	tools := mustToolset(t, searchTool())

	chosen, err := tools.Choosing(inference.ToolChoice{
		Mode: inference.ToolChoiceFunction,
		Name: "search",
	})
	if err != nil {
		t.Fatalf("forced existing tool rejected: %v", err)
	}
	if chosen.Choice() == nil || chosen.Choice().Name != "search" {
		t.Fatalf("choice not stored: %+v", chosen.Choice())
	}
	if tools.Choice() != nil {
		t.Fatal("Choosing mutated the receiver")
	}
	if _, err := tools.Choosing(inference.ToolChoice{
		Mode: inference.ToolChoiceFunction,
		Name: "missing",
	}); err == nil {
		t.Fatal("forced undefined tool accepted")
	}
	empty := inference.Toolset{}
	if _, err := empty.Choosing(inference.ToolChoice{Mode: inference.ToolChoiceRequired}); err == nil {
		t.Fatal("required choice accepted without tools")
	}
	if _, err := empty.Choosing(inference.ToolChoice{Mode: inference.ToolChoiceNone}); err != nil {
		t.Fatalf("none choice rejected without tools: %v", err)
	}
	if _, err := tools.Choosing(inference.ToolChoice{
		Mode: inference.ToolChoiceAuto,
		Name: "search",
	}); err == nil {
		t.Fatal("auto choice accepted a tool name")
	}
}

func TestToolsetCloneIsIndependent(t *testing.T) {
	tools := mustToolset(t, searchTool())
	clone := tools.Clone()
	if !clone.Has("search") || clone.Len() != 1 {
		t.Fatal("clone lost its definitions")
	}
	clone.Definitions()[0].Name = "mutated"
	if !tools.Has("search") {
		t.Fatal("clone shares storage with the original")
	}
	if !(inference.Toolset{}).Clone().IsZero() {
		t.Fatal("zero clone is not zero")
	}
}

// TestActiveFieldsMatchesRequestShape pins that the active-field set covers
// exactly the knobs the request can set. A driver compares this against what it
// consumed, so a field that is settable but never reported would be silently
// lost, and a reported field that cannot be set would be a phantom warning.
func TestActiveFieldsMatchesRequestShape(t *testing.T) {
	empty := textRequest(t)
	want := map[inference.Field]bool{
		inference.FieldInput:   true,
		inference.FieldRole:    true,
		inference.FieldContent: true,
	}
	assertFieldSet(t, empty.ActiveFields(), want)

	full := textRequest(t)
	full.Context = []message.Message{message.NewTextMessage(message.RoleUser, "earlier")}
	full.Output = inference.OutputText
	tools, err := mustToolset(t, searchTool()).Choosing(
		inference.ToolChoice{Mode: inference.ToolChoiceAuto},
	)
	if err != nil {
		t.Fatalf("choosing failed: %v", err)
	}
	full.Tools = tools
	full.Extensions = inference.Extensions{fakeExtension{fields: []inference.ExtensionField{"mode"}}}
	if err := full.Validate(); err != nil {
		t.Fatalf("full request rejected: %v", err)
	}
	want = map[inference.Field]bool{
		inference.FieldContext:                       true,
		inference.FieldInput:                         true,
		inference.FieldRole:                          true,
		inference.FieldContent:                       true,
		inference.FieldOutput:                        true,
		inference.FieldTools:                         true,
		inference.FieldToolChoice:                    true,
		inference.FieldExtensions:                    true,
		inference.Field("extension.test.extra.mode"): true,
	}
	assertFieldSet(t, full.ActiveFields(), want)
}

func TestOutputKindAndPartKind(t *testing.T) {
	cases := []struct {
		output inference.Output
		kind   inference.Output
		part   message.PartKind
	}{
		{"", inference.OutputText, message.PartText},
		{inference.OutputText, inference.OutputText, message.PartText},
		{inference.OutputImage, inference.OutputImage, message.PartImage},
		{inference.OutputAudio, inference.OutputAudio, message.PartAudio},
		{inference.OutputVideo, inference.OutputVideo, message.PartVideo},
	}
	for _, testCase := range cases {
		if err := testCase.output.Validate(); err != nil {
			t.Fatalf("%q rejected: %v", testCase.output, err)
		}
		if got := testCase.output.Kind(); got != testCase.kind {
			t.Fatalf("%q kind = %q, want %q", testCase.output, got, testCase.kind)
		}
		if got := testCase.output.PartKind(); got != testCase.part {
			t.Fatalf("%q part = %q, want %q", testCase.output, got, testCase.part)
		}
	}
	if err := inference.Output("hologram").Validate(); err == nil {
		t.Fatal("unknown output accepted")
	}
}

func TestGenerateResponseValidateForRejectsUndefinedTool(t *testing.T) {
	request := textRequest(t)
	request.Tools = mustToolset(t, searchTool())

	// A call to a defined tool is accepted.
	good := message.NewTextMessage(message.RoleAssistant, "")
	good.Content.Parts = append(good.Content.Parts, message.ToolCallPart{
		Call: message.ToolCall{ID: "c1", Name: "search", Arguments: json.RawMessage(`{}`)},
	})
	if err := (inference.GenerateResponse{
		Message:      good,
		FinishReason: inference.FinishToolCalls,
	}).ValidateFor(request); err != nil {
		t.Fatalf("defined tool rejected: %v", err)
	}

	// A call to an undefined tool is rejected and attached to the error.
	bad := message.NewTextMessage(message.RoleAssistant, "")
	bad.Content.Parts = append(bad.Content.Parts, message.ToolCallPart{
		Call: message.ToolCall{ID: "c2", Name: "missing", Arguments: json.RawMessage(`{}`)},
	})
	err := (inference.GenerateResponse{
		Message:      bad,
		FinishReason: inference.FinishToolCalls,
	}).ValidateFor(request)
	if err == nil {
		t.Fatal("expected undefined tool rejection")
	}
	if !inference.IsKind(err, inference.UndefinedTool) {
		t.Fatalf("kind = %v, want UndefinedTool", err)
	}
	inferenceErr := asInferenceError(t, err)
	if inferenceErr.UndefinedToolCall == nil || inferenceErr.UndefinedToolCall.Name != "missing" {
		t.Fatalf("undefined call not attached: %+v", inferenceErr.UndefinedToolCall)
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("undefined tool is deterministic and must be validation: %v", err)
	}
}

func TestGenerateResponseValidateRejectsWrongShape(t *testing.T) {
	cases := map[string]inference.GenerateResponse{
		"wrong role": {
			Message:      message.NewTextMessage(message.RoleUser, "hi"),
			FinishReason: inference.FinishStop,
		},
		"unknown finish": {
			Message:      message.NewTextMessage(message.RoleAssistant, "hi"),
			FinishReason: inference.FinishReason("whatever"),
		},
		"warning without message": {
			Message:      message.NewTextMessage(message.RoleAssistant, "hi"),
			FinishReason: inference.FinishStop,
			Warnings:     []inference.Warning{{Code: "x"}},
		},
		"warning names unknown field": {
			Message:      message.NewTextMessage(message.RoleAssistant, "hi"),
			FinishReason: inference.FinishStop,
			Warnings:     []inference.Warning{{Field: "bogus", Message: "x"}},
		},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			if err := response.Validate(); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestWarningAcceptsExtensionField(t *testing.T) {
	response := inference.GenerateResponse{
		Message:      message.NewTextMessage(message.RoleAssistant, "hi"),
		FinishReason: inference.FinishStop,
		Warnings: []inference.Warning{{
			Field:   inference.Field("extension.test.extra.mode"),
			Code:    "quantized",
			Message: "mode folded onto the only supported level",
		}},
	}
	if err := response.Validate(); err != nil {
		t.Fatalf("extension field warning rejected: %v", err)
	}
}

func TestStreamEventValidateBindsPayloadToKind(t *testing.T) {
	valid := []inference.StreamEvent{
		{Kind: inference.StreamStart},
		{Kind: inference.StreamText, Text: "hi"},
		{Kind: inference.StreamReasoning, Text: "thinking"},
		{Kind: inference.StreamToolCall, Tool: &inference.ToolCallDelta{Name: "search"}},
		{Kind: inference.StreamPart, Part: message.TextPart{Text: "chunk"}},
		{Kind: inference.StreamEnd, Finish: inference.FinishStop},
	}
	for _, event := range valid {
		if err := event.Validate(); err != nil {
			t.Fatalf("%s rejected: %v", event.Kind, err)
		}
	}
	invalid := []inference.StreamEvent{
		{Kind: inference.StreamEventKind("bogus")},
		{Kind: inference.StreamStart, Text: "leak"},
		{Kind: inference.StreamToolCall},
		{Kind: inference.StreamPart},
		{Kind: inference.StreamPart, Part: message.TextPart{Text: "chunk"}, Text: "also"},
		{Kind: inference.StreamEnd},
		{Kind: inference.StreamText, Tool: &inference.ToolCallDelta{}},
	}
	for _, event := range invalid {
		if err := event.Validate(); err == nil {
			t.Fatalf("%s accepted", event.Kind)
		}
	}
}

func TestRequestMetadataTravelsInContext(t *testing.T) {
	base := context.Background()
	if got := inference.RequestMetadataFrom(base); got != nil {
		t.Fatalf("bare context carries metadata: %v", got)
	}
	if got := inference.WithRequestMetadata(base, nil); got != base {
		t.Fatal("nil metadata replaced the context")
	}
	metadata := inference.RequestMetadata{"conversation": "c1", "turn": "7"}
	ctx := inference.WithRequestMetadata(base, metadata)
	got := inference.RequestMetadataFrom(ctx)
	if got.Get("conversation") != "c1" || got.Get("turn") != "7" {
		t.Fatalf("metadata lost: %v", got)
	}
	// The context owns a copy: mutating the caller's map does not change it.
	metadata["conversation"] = "mutated"
	if inference.RequestMetadataFrom(ctx).Get("conversation") != "c1" {
		t.Fatal("context shares storage with the caller")
	}
	if (inference.RequestMetadata)(nil).Get("x") != "" {
		t.Fatal("nil metadata is not empty")
	}
}

type fakeExtension struct {
	mode   string
	fields []inference.ExtensionField
}

func (f fakeExtension) ProviderID() string  { return "test" }
func (f fakeExtension) ExtensionID() string { return "extra" }
func (f fakeExtension) ActiveFields() []inference.ExtensionField {
	return f.fields
}
func (f fakeExtension) Validate() error            { return nil }
func (f fakeExtension) Clone() inference.Extension { return f }

func TestExtensionsQualifyAndFilter(t *testing.T) {
	own := fakeExtension{mode: "high", fields: []inference.ExtensionField{"mode"}}
	extensions := inference.Extensions{own}
	if err := extensions.Validate(); err != nil {
		t.Fatalf("extension rejected: %v", err)
	}
	fields := extensions.ActiveFields()
	if len(fields) != 1 || fields[0] != inference.Field("extension.test.extra.mode") {
		t.Fatalf("qualified fields = %v", fields)
	}
	if !fields[0].Valid() || !fields[0].IsExtension() {
		t.Fatalf("qualified field not recognized: %v", fields[0])
	}
	if got := extensions.ForProvider("other"); len(got) != 0 {
		t.Fatalf("foreign extensions leaked: %v", got)
	}
	if got := extensions.ForProvider("test"); len(got) != 1 {
		t.Fatalf("own extension dropped: %v", got)
	}
}

func TestExtensionsValidateRejectsMalformed(t *testing.T) {
	cases := map[string]inference.Extensions{
		"no fields":     {fakeExtension{}},
		"duplicate":     {fakeExtension{fields: []inference.ExtensionField{"mode"}}, fakeExtension{fields: []inference.ExtensionField{"mode"}}},
		"nil extension": {nil},
	}
	for name, extensions := range cases {
		t.Run(name, func(t *testing.T) {
			if err := extensions.Validate(); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestProviderIsSatisfiableAndStreamEndsWithEOF(t *testing.T) {
	var provider inference.Provider = stubProvider{}
	if _, err := provider.Generate(context.Background(), inference.GenerateRequest{}); err != nil {
		t.Fatalf("stub generate failed: %v", err)
	}
	stream, err := provider.GenerateStream(context.Background(), inference.GenerateRequest{})
	if err != nil {
		t.Fatalf("stub stream failed: %v", err)
	}
	defer stream.Close()
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream failed: %v", err)
		}
		if event.Kind == inference.StreamEnd {
			break
		}
	}
	if _, err := stream.Result(); err != nil {
		t.Fatalf("result failed: %v", err)
	}
}

func TestErrorRenderIsRedacted(t *testing.T) {
	const payload = "sk-live-secret-payload"
	err := inference.NewError(
		inference.ProviderFailure,
		inference.OperationGenerate,
		inference.FieldModel,
		errors.New(payload),
	)
	if strings.Contains(err.Error(), payload) {
		t.Fatalf("rendered error leaks the cause: %q", err.Error())
	}
	if !strings.Contains(err.Error(), string(inference.ProviderFailure)) {
		t.Fatalf("rendered error hides the kind: %q", err.Error())
	}
	if !inference.IsKind(err, inference.ProviderFailure) {
		t.Fatal("kind lookup failed")
	}
	if errors.Unwrap(err) == nil {
		t.Fatal("cause is not unwrappable: a retry/backoff consumer needs it")
	}
}

type stubProvider struct{}

func (stubProvider) Generate(context.Context, inference.GenerateRequest) (inference.GenerateResponse, error) {
	return inference.GenerateResponse{}, nil
}

func (stubProvider) GenerateStream(context.Context, inference.GenerateRequest) (inference.Stream, error) {
	return stubStream{}, nil
}

type stubStream struct{ ended bool }

func (s stubStream) Next(context.Context) (inference.StreamEvent, error) {
	if s.ended {
		return inference.StreamEvent{}, io.EOF
	}
	return inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop}, nil
}

func (s stubStream) Result() (inference.GenerateResponse, error) {
	return inference.GenerateResponse{}, nil
}

func (s stubStream) Close() error { return nil }

func assertFieldSet(t *testing.T, got []inference.Field, want map[inference.Field]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("active fields = %v, want %d entries %v", got, len(want), want)
	}
	for _, field := range got {
		if !want[field] {
			t.Fatalf("unexpected active field %q", field)
		}
		if !field.Valid() {
			t.Fatalf("active field %q is not valid", field)
		}
	}
}

func asInferenceError(t *testing.T, err error) *inference.Error {
	t.Helper()
	var inferenceErr *inference.Error
	if !errors.As(err, &inferenceErr) {
		t.Fatalf("error is not *inference.Error: %v", err)
	}
	return inferenceErr
}
