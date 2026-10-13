package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	"github.com/openai/openai-go/v3"
)

// decisionRequestFixture is the request every decision test starts from: the
// three question kinds, plus the typed pair a choice question must keep apart
// — the string "true" and the boolean true are two options, not one.
func decisionRequestFixture() inference.DecisionRequest {
	return inference.DecisionRequest{
		Input: inference.DecisionInput{Text: "the evidence"},
		Questions: []inference.DecisionQuestion{
			{
				Name:         "spam",
				Kind:         inference.DecisionQuestionPredicate,
				Instructions: "Is this message spam?",
			},
			{
				Name:         "route",
				Kind:         inference.DecisionQuestionChoice,
				Instructions: "Which queue should handle this?",
				Choices: []inference.DecisionChoice{
					{Value: inference.NewDecisionStringChoice("billing")},
					{
						Value:       inference.NewDecisionStringChoice("true"),
						Description: "the literal string",
					},
					{
						Value:       inference.NewDecisionBooleanChoice(true),
						Description: "the boolean",
					},
				},
			},
			{
				Name:         "confidence",
				Kind:         inference.DecisionQuestionScore,
				Instructions: "How confident is the classification?",
				Levels: []inference.DecisionLevel{
					{Label: "low"},
					{Label: "medium", Description: "usable with review"},
					{Label: "high"},
				},
			},
		},
	}
}

// decisionResponseJSON is one Decisions response in the surface's own shape:
// an answer per question in order, a typed choice distribution, a score
// distribution, and the five usage counters.
func decisionResponseJSON() string {
	return `{
		"model": "gpt-6-luna-2026-09-01",
		"answers": [
			{"type": "predicate", "name": "spam", "probability": 0.92},
			{"type": "choice", "name": "route", "choice": "billing",
			 "confidence": 0.71,
			 "probabilities": [
				{"value": "billing", "probability": 0.71},
				{"value": "true", "probability": 0.2},
				{"value": true, "probability": 0.09}
			 ]},
			{"type": "score", "name": "confidence", "score": 1.4,
			 "confidence": 0.6,
			 "probabilities": [
				{"value": 0, "label": "low", "probability": 0.1},
				{"value": 1, "label": "medium", "probability": 0.4},
				{"value": 2, "label": "high", "probability": 0.5}
			 ]}
		],
		"usage": {
			"input_tokens": 120, "output_tokens": 30, "total_tokens": 150,
			"input_tokens_details": {"cached_tokens": 20, "cache_write_tokens": 12},
			"output_tokens_details": {"reasoning_tokens": 8}
		}
	}`
}

// decisionPath is the route the standalone Decisions surface serves, relative
// to the endpoint root. The SDK owns it and the driver never states it, so the
// offline suites pin it here: a dependency bump that renamed the surface would
// otherwise redden only the gated live lane.
const decisionPath = "/decisions"

// assertDecisionRoute pins the method and route one captured request arrived
// on.
func assertDecisionRoute(t *testing.T, r *http.Request) {
	t.Helper()
	if r.Method != http.MethodPost || r.URL.Path != decisionPath {
		t.Errorf("call = %s %s, want POST %s", r.Method, r.URL.Path, decisionPath)
	}
}

// decisionBody renders one compiled request the way it reaches the wire.
func decisionBody(t *testing.T, params openai.DecisionNewParams) map[string]any {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("params are not JSON: %v", err)
	}
	return body
}

func TestConformanceDecisionCompiler(t *testing.T) {
	inferencetest.RunCompiler(t, inferencetest.CompilerSuite[inference.DecisionRequest, openai.DecisionNewParams]{
		Operation: model.OperationDecision,
		Model:     openaiModel("gpt-6-luna"),
		Request:   decisionRequestFixture,
		Snapshot: func(request inference.DecisionRequest) any {
			return request.Clone()
		},
		Fields: func(request inference.DecisionRequest) []inference.FieldID {
			return request.ActiveFields()
		},
		Compile: compileDecisionFor("gpt-6-luna", declarations["gpt-6-luna"]),
		AssertWire: func(t *testing.T, params openai.DecisionNewParams) {
			body := decisionBody(t, params)
			if body["model"] != "gpt-6-luna" || body["input"] != "the evidence" {
				t.Fatalf("body = %+v", body)
			}
			questions, ok := body["questions"].([]any)
			if !ok || len(questions) != 3 {
				t.Fatalf("questions = %#v", body["questions"])
			}
			predicate := questions[0].(map[string]any)
			if predicate["type"] != "predicate" || predicate["name"] != "spam" ||
				predicate["instructions"] != "Is this message spam?" {
				t.Fatalf("predicate = %+v", predicate)
			}
			choice := questions[1].(map[string]any)
			if choice["type"] != "choice" || choice["name"] != "route" {
				t.Fatalf("choice = %+v", choice)
			}
			choices := choice["choices"].([]any)
			if len(choices) != 3 {
				t.Fatalf("choices = %#v", choice["choices"])
			}
			// The typed pair survives lowering: "true" stays a string and
			// true stays a boolean.
			if choices[1].(map[string]any)["value"] != "true" ||
				choices[1].(map[string]any)["description"] != "the literal string" {
				t.Fatalf("string choice = %+v", choices[1])
			}
			if choices[2].(map[string]any)["value"] != true ||
				choices[2].(map[string]any)["description"] != "the boolean" {
				t.Fatalf("boolean choice = %+v", choices[2])
			}
			// A description the caller never declared stays off the wire: the
			// surface reads an empty string as a declared rubric, which is the
			// one shape decisionOptional exists to prevent.
			if _, ok := choices[0].(map[string]any)["description"]; ok {
				t.Fatalf("undeclared choice description rode the wire: %+v", choices[0])
			}
			score := questions[2].(map[string]any)
			levels := score["levels"].([]any)
			if score["type"] != "score" || len(levels) != 3 {
				t.Fatalf("score = %+v", score)
			}
			if levels[1].(map[string]any)["label"] != "medium" ||
				levels[1].(map[string]any)["description"] != "usable with review" {
				t.Fatalf("level = %+v", levels[1])
			}
			for _, index := range []int{0, 2} {
				if _, ok := levels[index].(map[string]any)["description"]; ok {
					t.Fatalf("undeclared level description rode the wire: %+v",
						levels[index])
				}
			}
			if _, ok := body["safety_identifier"]; ok {
				t.Fatalf("safety_identifier rides without an extension: %+v", body)
			}
		},
		Rejections: []inferencetest.CompilerRejection[inference.DecisionRequest]{
			{
				Name: "request metadata",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					request.RequestMetadata = map[string]string{"trace": "abc"}
					return request
				},
				Field: inference.FieldDecisionRequestMetadata,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "too many questions",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					question := request.Questions[0]
					request.Questions = nil
					for index := 0; index <= decisionMaxQuestions; index++ {
						question.Name = fmt.Sprintf("q%d", index)
						request.Questions = append(request.Questions, question)
					}
					return request
				},
				Field: inference.FieldDecisionQuestionKind,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "too many choices",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					choices := make([]inference.DecisionChoice, 0, decisionMaxChoices+1)
					for index := 0; index <= decisionMaxChoices; index++ {
						choices = append(choices, inference.DecisionChoice{
							Value: inference.NewDecisionStringChoice(
								fmt.Sprintf("option-%d", index)),
						})
					}
					request.Questions[1].Choices = choices
					return request
				},
				Field: inference.FieldDecisionQuestionChoices,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "too many levels",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					levels := make([]inference.DecisionLevel, 0, decisionMaxLevels+1)
					for index := 0; index <= decisionMaxLevels; index++ {
						levels = append(levels, inference.DecisionLevel{
							Label: fmt.Sprintf("level-%d", index),
						})
					}
					request.Questions[2].Levels = levels
					return request
				},
				Field: inference.FieldDecisionQuestionLevels,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "unknown question kind",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					request.Questions[0].Kind = "certainty"
					return request
				},
				Field: inference.FieldDecisionQuestionKind,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "oversized text evidence",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					request.Input = inference.DecisionInput{
						Text: strings.Repeat("a", decisionMaxInputBytes+1),
					}
					return request
				},
				Field: inference.FieldDecisionInputText,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "oversized json evidence",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					request.Input = inference.DecisionInput{
						JSON: json.RawMessage(
							`{"evidence":"` +
								strings.Repeat("a", decisionMaxInputBytes) + `"}`),
					}
					return request
				},
				Field: inference.FieldDecisionInputJSON,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "too many images",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					parts := make([]message.Part, 0, decisionMaxImages+1)
					for index := 0; index <= decisionMaxImages; index++ {
						image, err := media.NewImageURL(
							fmt.Sprintf("https://example.com/%d.png", index),
							"image/png",
						)
						if err != nil {
							t.Fatal(err)
						}
						parts = append(parts, message.ImagePart{Source: image})
					}
					request.Input = inference.DecisionInput{Parts: parts}
					return request
				},
				Field: inference.FieldDecisionInputPartImage,
				Kind:  inference.UnsupportedFeature,
			},
			{
				// The parts carrier draws on the same byte budget as the text
				// and json carriers: neither part is oversized on its own, so
				// only the aggregate check refuses this request.
				Name: "text parts past the input bound",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					half := strings.Repeat("a", decisionMaxInputBytes/2+1)
					request.Input = inference.DecisionInput{
						Parts: []message.Part{
							message.TextPart{Text: half},
							message.TextPart{Text: half},
						},
					}
					return request
				},
				Field: inference.FieldDecisionInputPartText,
				Kind:  inference.UnsupportedFeature,
			},
			{
				// An inline image is sent as a data URI about a third larger
				// than the bytes it holds, which is what the bound counts:
				// the source below is under the bound and the part is not.
				Name: "inline image past the input bound",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					source, err := media.NewImageBytes(
						bytes.Repeat([]byte{0xAB}, decisionMaxInputBytes/4*3+1<<20),
						"image/png",
					)
					if err != nil {
						t.Fatal(err)
					}
					request.Input = inference.DecisionInput{
						Parts: []message.Part{message.ImagePart{Source: source}},
					}
					return request
				},
				Field: inference.FieldDecisionInputPartImage,
				Kind:  inference.UnsupportedFeature,
			},
			{
				Name: "extension for another operation",
				Request: func() inference.DecisionRequest {
					request := decisionRequestFixture()
					request.Extensions = inference.Extensions{
						ImageOptions{Background: "transparent"},
					}
					return request
				},
				Field: inference.ExtensionField("background").Qualify(
					ImageOptions{Background: "transparent"}),
				Kind: inference.InvalidExtension,
			},
		},
	})
}

// TestConformanceDecisionCompilerWithoutImageInput pins the capability
// boundary: image evidence is refused at compile time on a target that does
// not declare image input, instead of failing on every request.
func TestConformanceDecisionCompilerWithoutImageInput(t *testing.T) {
	inferencetest.RunCompiler(t, inferencetest.CompilerSuite[inference.DecisionRequest, openai.DecisionNewParams]{
		Operation: model.OperationDecision,
		Model:     openaiModel("gpt-6-luna-text"),
		Request: func() inference.DecisionRequest {
			request := decisionRequestFixture()
			request.Questions = request.Questions[:1]
			return request
		},
		Snapshot: func(request inference.DecisionRequest) any {
			return request.Clone()
		},
		Fields: func(request inference.DecisionRequest) []inference.FieldID {
			return request.ActiveFields()
		},
		Compile: compileDecisionFor("gpt-6-luna-text", declarations["gpt-6-luna-text"]),
		Rejections: []inferencetest.CompilerRejection[inference.DecisionRequest]{
			{
				Name: "image evidence on a text-only model",
				Request: func() inference.DecisionRequest {
					image, err := media.NewImageURL(
						"https://example.com/i.png", "image/png")
					if err != nil {
						t.Fatal(err)
					}
					request := decisionRequestFixture()
					request.Questions = request.Questions[:1]
					request.Input = inference.DecisionInput{
						Parts: []message.Part{message.ImagePart{Source: image}},
					}
					return request
				},
				Field: inference.FieldDecisionInputPartImage,
				Kind:  inference.UnsupportedFeature,
			},
		},
	})
}

func TestConformanceDecisionUnary(t *testing.T) {
	server, _ := newCapturedOpenAI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		assertDecisionRoute(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, decisionResponseJSON())
	})
	defer server.Close()
	calls := &inferencetest.Counter{}
	cls := testClients(t, server)
	// Bind the pipeline directly so the transport probe sits inside the
	// bound driver, the way the generate suites do it.
	driver, err := inference.BindDecision(
		compileDecisionFor("gpt-6-luna", declarations["gpt-6-luna"]),
		countingTransport(calls, transportDecision(cls.api)),
		decodeDecision,
	)
	if err != nil {
		t.Fatalf("BindDecision: %v", err)
	}
	inferencetest.RunDecisionUnary(t, inferencetest.DecisionUnarySuite{
		Model:   openaiModel("gpt-6-luna"),
		Request: decisionRequestFixture,
		Driver:  driver,
		TransportCalls: func() int64 {
			return calls.Load()
		},
	})
}

// TestDecisionWire pins the whole path once: the request body the surface
// receives, and the canonical answers the response decodes into.
func TestDecisionWire(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		assertDecisionRoute(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, decisionResponseJSON())
	})
	defer server.Close()
	cls := testClients(t, server)
	driver, err := openDecisionFor(
		cls,
		declarations["gpt-6-luna"],
		model.ModelID{Provider: "openai", Name: "gpt-6-luna"},
		"default",
	)
	if err != nil {
		t.Fatalf("openDecision: %v", err)
	}

	request := decisionRequestFixture()
	request.Extensions = inference.Extensions{
		DecisionOptions{SafetyIdentifier: "user-hash"},
	}
	response, err := driver.Execute(
		context.Background(), openaiModel("gpt-6-luna"), request)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	body := capture.body(0)
	if body["input"] != "the evidence" ||
		body["safety_identifier"] != "user-hash" {
		t.Fatalf("body = %+v", body)
	}

	if response.Model != "gpt-6-luna-2026-09-01" {
		t.Fatalf("model = %q", response.Model)
	}
	if len(response.Answers) != 3 {
		t.Fatalf("answers = %d", len(response.Answers))
	}
	predicate := response.Answers[0]
	if predicate.Name != "spam" ||
		predicate.Kind != inference.DecisionAnswerPredicate ||
		predicate.Probability == nil || *predicate.Probability != 0.92 {
		t.Fatalf("predicate = %+v", predicate)
	}
	choice := response.Answers[1]
	if choice.Name != "route" || choice.Kind != inference.DecisionAnswerChoice {
		t.Fatalf("choice = %+v", choice)
	}
	if choice.Choice == nil ||
		*choice.Choice != inference.NewDecisionStringChoice("billing") {
		t.Fatalf("chosen value = %+v", choice.Choice)
	}
	if len(choice.Probabilities) != 3 {
		t.Fatalf("distribution = %+v", choice.Probabilities)
	}
	// The typed pair survives the round trip in both directions: index 1 is
	// the string "true" and index 2 is the boolean true, and neither is
	// coerced into the other on the way back.
	if value := choice.Probabilities[1].Value; value != inference.NewDecisionStringChoice("true") {
		t.Fatalf("string option = %+v", value)
	}
	if value := choice.Probabilities[2].Value; value != inference.NewDecisionBooleanChoice(true) {
		t.Fatalf("typed option = %+v", value)
	}
	score := response.Answers[2]
	if score.Kind != inference.DecisionAnswerScore ||
		score.Score == nil || *score.Score != 1.4 {
		t.Fatalf("score = %+v", score)
	}
	if len(score.Levels) != 3 || score.Levels[1].Label != "medium" ||
		score.Levels[1].Level != 1 {
		t.Fatalf("levels = %+v", score.Levels)
	}

	usage := response.Usage
	if usage.InputTokens != 120 || usage.OutputTokens != 30 ||
		usage.TotalTokens != 150 {
		t.Fatalf("usage = %+v", usage)
	}
	if usage.Input.CacheReadTokens == nil || *usage.Input.CacheReadTokens != 20 {
		t.Fatalf("cache reads = %+v", usage.Input)
	}
	if usage.Input.CacheWriteTokens == nil || *usage.Input.CacheWriteTokens != 12 {
		t.Fatalf("cache writes = %+v", usage.Input)
	}
	if usage.Output.ReasoningTokens == nil || *usage.Output.ReasoningTokens != 8 {
		t.Fatalf("reasoning tokens = %+v", usage.Output)
	}
}

// TestDecisionInputCarriers pins how each canonical carrier reaches the wire:
// structured evidence as its serialized text, and parts as one user message
// whose images are the URIs the wire accepts.
func TestDecisionInputCarriers(t *testing.T) {
	target := declarations["gpt-6-luna"]
	compile := compileDecisionFor("gpt-6-luna", target)

	t.Run("json evidence", func(t *testing.T) {
		request := decisionRequestFixture()
		request.Input = inference.DecisionInput{
			JSON: json.RawMessage(`{"order":{"id":7}}`),
		}
		compiled, err := compile(context.Background(), openaiModel("gpt-6-luna"), request)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		if body := decisionBody(t, compiled.Wire); body["input"] != `{"order":{"id":7}}` {
			t.Fatalf("input = %#v", body["input"])
		}
	})

	t.Run("parts", func(t *testing.T) {
		linked, err := media.NewImageURL("https://example.com/i.png", "image/png")
		if err != nil {
			t.Fatal(err)
		}
		inline, err := media.NewImageBytes([]byte("png"), "image/png")
		if err != nil {
			t.Fatal(err)
		}
		request := decisionRequestFixture()
		request.Input = inference.DecisionInput{
			Parts: []message.Part{
				message.TextPart{Text: "look at these"},
				message.ImagePart{Source: linked},
				message.ImagePart{Source: inline},
			},
		}
		compiled, err := compile(context.Background(), openaiModel("gpt-6-luna"), request)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		body := decisionBody(t, compiled.Wire)
		input, ok := body["input"].([]any)
		if !ok || len(input) != 1 {
			t.Fatalf("input = %#v", body["input"])
		}
		messageBody := input[0].(map[string]any)
		if messageBody["type"] != "message" || messageBody["role"] != "user" {
			t.Fatalf("message = %+v", messageBody)
		}
		parts := messageBody["content"].([]any)
		if len(parts) != 3 {
			t.Fatalf("parts = %#v", messageBody["content"])
		}
		if parts[0].(map[string]any)["type"] != "input_text" ||
			parts[0].(map[string]any)["text"] != "look at these" {
			t.Fatalf("text part = %+v", parts[0])
		}
		linkedPart := parts[1].(map[string]any)
		if linkedPart["type"] != "input_image" ||
			linkedPart["image_url"] != "https://example.com/i.png" {
			t.Fatalf("linked image = %+v", linkedPart)
		}
		inlinePart := parts[2].(map[string]any)
		if inlinePart["image_url"] != "data:image/png;base64,cG5n" {
			t.Fatalf("inline image = %+v", inlinePart)
		}
	})
}

// TestDecisionUnsupportedPartKindRefused pins the refusal for a part kind a
// decision input cannot carry. Core refuses such a kind before the compiler
// runs (its part table has rows for text and image only, and DecisionInput
// .Validate rejects the rest), so this drives the compiler itself — the way
// the unary suite and the conformance harness do — and pins what those callers
// get: a structured rejection on a field the request really carries, not a
// panic out of the generate part table, whose miss means vocabulary drift
// rather than a kind this wire cannot send.
func TestDecisionUnsupportedPartKindRefused(t *testing.T) {
	compile := compileDecisionFor("gpt-6-luna", declarations["gpt-6-luna"])

	t.Run("with a part field to name", func(t *testing.T) {
		request := decisionRequestFixture()
		request.Input = inference.DecisionInput{
			Parts: []message.Part{
				message.TextPart{Text: "the evidence"},
				message.AudioPart{},
			},
		}
		compiled, err := compile(
			context.Background(), openaiModel("gpt-6-luna"), request)
		if err == nil {
			t.Fatal("Compile accepted audio evidence")
		}
		if !inference.IsKind(err, inference.UnsupportedFeature) {
			t.Fatalf("error = %v, want unsupported_feature", err)
		}
		var inferenceErr *inference.Error
		if !errors.As(err, &inferenceErr) ||
			inferenceErr.Field != inference.FieldDecisionInputPartText {
			t.Fatalf("error context = %+v, want the text part field", inferenceErr)
		}
		// The reason names the part, and it rides the cause rather than
		// Error()'s redacted text.
		cause := errors.Unwrap(err)
		if cause == nil ||
			!strings.Contains(cause.Error(), "audio evidence is not supported") {
			t.Fatalf("cause = %v, want the refused kind in it", cause)
		}
		// The refusal lands on a field the request activates, so the report
		// stays one the runtime accepts for a failed compile.
		if err := compiled.Report.ValidateFailure(
			model.OperationDecision, request.ActiveFields(),
		); err != nil {
			t.Fatalf("rejection report: %v", err)
		}
	})

	t.Run("with no part field at all", func(t *testing.T) {
		// A parts carrier made only of kinds the surface cannot carry
		// activates no part field, so there is no field of its own to report
		// the refusal on; the text part field stands in. Core's validator is
		// what keeps this out of the runtime, and the driver must still not
		// panic on the way there.
		request := decisionRequestFixture()
		request.Input = inference.DecisionInput{
			Parts: []message.Part{message.VideoPart{}},
		}
		_, err := compile(context.Background(), openaiModel("gpt-6-luna"), request)
		if err == nil {
			t.Fatal("Compile accepted video evidence")
		}
		var inferenceErr *inference.Error
		if !errors.As(err, &inferenceErr) ||
			inferenceErr.Field != inference.FieldDecisionInputPartText {
			t.Fatalf("error context = %+v, want the text part field", inferenceErr)
		}
	})
}

// TestDecisionBoundsAtTheCeiling pins the other side of every bound: a request
// that sits exactly on the surface's maximum compiles and reaches the wire
// whole. The refusals next door only prove the checks reject past the bound, so
// a comparison flipped to `>=` — refusing the documented maximum — would keep
// the rest of the suite green.
func TestDecisionBoundsAtTheCeiling(t *testing.T) {
	compile := compileDecisionFor("gpt-6-luna", declarations["gpt-6-luna"])
	compileRequest := func(
		t *testing.T,
		request inference.DecisionRequest,
	) openai.DecisionNewParams {
		t.Helper()
		compiled, err := compile(
			context.Background(), openaiModel("gpt-6-luna"), request)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		return compiled.Wire
	}

	t.Run("questions", func(t *testing.T) {
		request := inference.DecisionRequest{
			Input: inference.DecisionInput{Text: "the evidence"},
		}
		for index := 0; index < decisionMaxQuestions; index++ {
			request.Questions = append(request.Questions, inference.DecisionQuestion{
				Name:         fmt.Sprintf("q%d", index),
				Kind:         inference.DecisionQuestionPredicate,
				Instructions: "Is this true?",
			})
		}
		body := decisionBody(t, compileRequest(t, request))
		questions, ok := body["questions"].([]any)
		if !ok || len(questions) != decisionMaxQuestions {
			t.Fatalf("questions = %d, want %d", len(questions), decisionMaxQuestions)
		}
	})

	t.Run("choices", func(t *testing.T) {
		request := decisionRequestFixture()
		choices := make([]inference.DecisionChoice, 0, decisionMaxChoices)
		for index := 0; index < decisionMaxChoices; index++ {
			choices = append(choices, inference.DecisionChoice{
				Value: inference.NewDecisionStringChoice(fmt.Sprintf("option-%d", index)),
			})
		}
		request.Questions[1].Choices = choices
		body := decisionBody(t, compileRequest(t, request))
		compiled := body["questions"].([]any)[1].(map[string]any)["choices"].([]any)
		if len(compiled) != decisionMaxChoices {
			t.Fatalf("choices = %d, want %d", len(compiled), decisionMaxChoices)
		}
	})

	t.Run("levels", func(t *testing.T) {
		request := decisionRequestFixture()
		levels := make([]inference.DecisionLevel, 0, decisionMaxLevels)
		for index := 0; index < decisionMaxLevels; index++ {
			levels = append(levels, inference.DecisionLevel{
				Label: fmt.Sprintf("level-%d", index),
			})
		}
		request.Questions[2].Levels = levels
		body := decisionBody(t, compileRequest(t, request))
		compiled := body["questions"].([]any)[2].(map[string]any)["levels"].([]any)
		if len(compiled) != decisionMaxLevels {
			t.Fatalf("levels = %d, want %d", len(compiled), decisionMaxLevels)
		}
	})

	t.Run("images", func(t *testing.T) {
		request := decisionRequestFixture()
		parts := make([]message.Part, 0, decisionMaxImages)
		for index := 0; index < decisionMaxImages; index++ {
			source, err := media.NewImageURL(
				fmt.Sprintf("https://example.com/%d.png", index), "image/png")
			if err != nil {
				t.Fatal(err)
			}
			parts = append(parts, message.ImagePart{Source: source})
		}
		request.Input = inference.DecisionInput{Parts: parts}
		body := decisionBody(t, compileRequest(t, request))
		compiled := body["input"].([]any)[0].(map[string]any)["content"].([]any)
		if len(compiled) != decisionMaxImages {
			t.Fatalf("images = %d, want %d", len(compiled), decisionMaxImages)
		}
	})

	t.Run("input bytes", func(t *testing.T) {
		request := decisionRequestFixture()
		request.Input = inference.DecisionInput{
			Text: strings.Repeat("a", decisionMaxInputBytes),
		}
		body := decisionBody(t, compileRequest(t, request))
		evidence, ok := body["input"].(string)
		if !ok || len(evidence) != decisionMaxInputBytes {
			t.Fatalf("input = %d bytes, want %d", len(evidence), decisionMaxInputBytes)
		}
	})
}

// TestDecisionDecode pins the strict reading of one answer at a time: what the
// decoder refuses, and the one kind that carries no value (refusal).
func TestDecisionDecode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer string
		want   string
	}{
		{
			name:   "null name",
			answer: `{"type":"predicate","name":null,"probability":0.5}`,
			want:   "carries no name",
		},
		{
			name:   "unknown type",
			answer: `{"type":"certainty","name":"q"}`,
			want:   `unknown decision answer type "certainty"`,
		},
		{
			name:   "predicate without a probability",
			answer: `{"type":"predicate","name":"q"}`,
			want:   "carries no probability",
		},
		{
			name: "choice value of an unsupported type",
			answer: `{"type":"choice","name":"q","choice":42,"confidence":0.5,` +
				`"probabilities":[{"value":42,"probability":1}]}`,
			want: "neither a string nor a boolean",
		},
		{
			name: "choice distribution that does not sum",
			answer: `{"type":"choice","name":"q","choice":"a","confidence":0.9,` +
				`"probabilities":[{"value":"a","probability":0.5},` +
				`{"value":"b","probability":0.2}]}`,
			want: "sums to 0.7, not 1",
		},
		{
			name: "choice without a distribution",
			answer: `{"type":"choice","name":"q","choice":"a","confidence":0.9,` +
				`"probabilities":[]}`,
			want: "carries no distribution",
		},
		{
			// A missing chosen value is the surface's own contract
			// violation: the value check reports it as such rather than as
			// the parse error a null would look like.
			name: "choice without a chosen value",
			answer: `{"type":"choice","name":"q","choice":null,"confidence":0.9,` +
				`"probabilities":[{"value":"a","probability":1}]}`,
			want: "carries no chosen value",
		},
		{
			name: "choice without the chosen field at all",
			answer: `{"type":"choice","name":"q","confidence":0.9,` +
				`"probabilities":[{"value":"a","probability":1}]}`,
			want: "carries no chosen value",
		},
		{
			name: "choice option without a value",
			answer: `{"type":"choice","name":"q","choice":"a","confidence":0.9,` +
				`"probabilities":[{"probability":1}]}`,
			want: "option 0 carries no value",
		},
		{
			name: "choice distribution rounded past the tolerance",
			answer: `{"type":"choice","name":"q","choice":"a","confidence":0.9,` +
				`"probabilities":[{"value":"a","probability":0.334},` +
				`{"value":"b","probability":0.334},` +
				`{"value":"c","probability":0.334}]}`,
			want: "sums to 1.002",
		},
		{
			name: "choice without a confidence",
			answer: `{"type":"choice","name":"q","choice":"a",` +
				`"probabilities":[{"value":"a","probability":1}]}`,
			want: "carries no confidence",
		},
		{
			name: "choice option value of an unsupported type",
			answer: `{"type":"choice","name":"q","choice":"a","confidence":0.9,` +
				`"probabilities":[{"value":["a"],"probability":1}]}`,
			want: `option 0: choice value ["a"] is neither a string nor a boolean`,
		},
		{
			name: "score without a score",
			answer: `{"type":"score","name":"q","confidence":0.5,` +
				`"probabilities":[{"value":0,"label":"low","probability":1}]}`,
			want: "carries no score",
		},
		{
			name: "score level outside the index range",
			answer: `{"type":"score","name":"q","score":0,"confidence":0.5,` +
				`"probabilities":[{"value":-1,"label":"low","probability":1}]}`,
			want: "names index -1",
		},
		{
			name: "score level beyond the index range",
			answer: `{"type":"score","name":"q","score":0,"confidence":0.5,` +
				`"probabilities":[{"value":4294967296,"label":"low","probability":1}]}`,
			want: "names index 4294967296",
		},
		{
			// The index places the level: read as 0, a dropped one would
			// answer a question the caller can act on.
			name: "score level without an index",
			answer: `{"type":"score","name":"q","score":0,"confidence":0.5,` +
				`"probabilities":[{"label":"low","probability":1}]}`,
			want: "level 0 carries no index",
		},
		{
			name: "score without a confidence",
			answer: `{"type":"score","name":"q","score":0,` +
				`"probabilities":[{"value":0,"label":"low","probability":1}]}`,
			want: "carries no confidence",
		},
		{
			name: "score distribution that does not sum",
			answer: `{"type":"score","name":"q","score":0.9,"confidence":0.5,` +
				`"probabilities":[{"value":0,"label":"low","probability":0.5},` +
				`{"value":1,"label":"high","probability":0.4}]}`,
			want: "sums to 0.9, not 1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var union openai.DecisionAnswerUnion
			if err := json.Unmarshal([]byte(tc.answer), &union); err != nil {
				t.Fatalf("fixture is not a decision answer: %v", err)
			}
			response, err := decodeDecision(
				context.Background(),
				decisionRaw{answers: []openai.DecisionAnswerUnion{union}},
			)
			if err == nil {
				t.Fatalf("decode accepted %s: %+v", tc.answer, response)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}

	t.Run("refusal", func(t *testing.T) {
		var union openai.DecisionAnswerUnion
		if err := json.Unmarshal(
			[]byte(`{"type":"refusal","name":"route"}`), &union); err != nil {
			t.Fatalf("fixture is not a decision answer: %v", err)
		}
		response, err := decodeDecision(
			context.Background(),
			decisionRaw{answers: []openai.DecisionAnswerUnion{union}},
		)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		answer := response.Answers[0]
		if answer.Kind != inference.DecisionAnswerRefusal || answer.Name != "route" {
			t.Fatalf("answer = %+v", answer)
		}
		if answer.Probability != nil || answer.Choice != nil ||
			answer.Confidence != nil || answer.Score != nil ||
			len(answer.Probabilities) != 0 || len(answer.Levels) != 0 {
			t.Fatalf("refusal carries another kind's fields: %+v", answer)
		}
	})

	t.Run("distributions within the tolerance", func(t *testing.T) {
		// An option rounded to three decimals is what the tolerance exists
		// for: thirds sum to 0.999, and a wire that rounds each one is not a
		// contract violation. Tightening the constant to an exact sum would
		// redden every real response, so the accepted side is pinned here.
		for _, answer := range []string{
			`{"type":"choice","name":"q","choice":"a","confidence":0.9,` +
				`"probabilities":[{"value":"a","probability":0.333},` +
				`{"value":"b","probability":0.333},` +
				`{"value":"c","probability":0.333}]}`,
			`{"type":"score","name":"q","score":0.9,"confidence":0.5,` +
				`"probabilities":[{"value":0,"label":"low","probability":0.333},` +
				`{"value":1,"label":"mid","probability":0.333},` +
				`{"value":2,"label":"high","probability":0.333}]}`,
			// A surface that reports no labels is accepted: core reconciles
			// a non-empty label against the question and tolerates an empty
			// one, so refusing a missing label would make the driver
			// stricter than the layer it feeds without protecting anything.
			// The index is the placement fact, and that one is required.
			`{"type":"score","name":"q","score":0.9,"confidence":0.5,` +
				`"probabilities":[{"value":0,"probability":1}]}`,
		} {
			var union openai.DecisionAnswerUnion
			if err := json.Unmarshal([]byte(answer), &union); err != nil {
				t.Fatalf("fixture is not a decision answer: %v", err)
			}
			response, err := decodeDecision(
				context.Background(),
				decisionRaw{answers: []openai.DecisionAnswerUnion{union}},
			)
			if err != nil {
				t.Fatalf("decode refused a rounded distribution: %v", err)
			}
			if len(response.Answers) != 1 {
				t.Fatalf("answers = %+v", response.Answers)
			}
		}
	})
}

// TestDecisionDecodeErrorEchoIsBounded pins that the provider text a decode
// error repeats is a fragment. The answer's position and the question's name
// already locate the failure, and this framework treats a decode error's cause
// as diagnostic text, so a provider must not be able to put its whole payload
// into a deployment's log line.
func TestDecisionDecodeErrorEchoIsBounded(t *testing.T) {
	long := strings.Repeat("a", 4096)
	// A value of an unsupported type is the reachable way to a long echo: the
	// surface's own decoding produces a value that parses as JSON but is
	// neither a string nor a boolean.
	array := "[" + strings.Repeat("1,", 2048) + "1]"
	for _, tc := range []struct {
		name string
		// answer is the wire answer, and echoed is the provider text the
		// decoder reads out of it, which is what the error must repeat only
		// in part.
		answer string
		echoed string
	}{
		{
			name:   "answer type",
			answer: `{"type":"` + long + `","name":"q"}`,
			echoed: long,
		},
		{
			name: "option value",
			answer: `{"type":"choice","name":"q","choice":"a","confidence":0.5,` +
				`"probabilities":[{"value":` + array + `,"probability":1}]}`,
			echoed: array,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var union openai.DecisionAnswerUnion
			if err := json.Unmarshal([]byte(tc.answer), &union); err != nil {
				t.Fatalf("fixture is not a decision answer: %v", err)
			}
			_, err := decodeDecision(
				context.Background(),
				decisionRaw{answers: []openai.DecisionAnswerUnion{union}},
			)
			if err == nil {
				t.Fatal("decode accepted the answer")
			}
			message := err.Error()
			if !strings.Contains(message, tc.echoed[:decisionEchoBytes]) {
				t.Errorf("error = %q, want the head of the provider value", message)
			}
			if strings.Contains(message, tc.echoed[:decisionEchoBytes+1]) {
				t.Errorf("error repeats more than %d bytes: %q",
					decisionEchoBytes, message)
			}
		})
	}
}

// TestDecisionTransportErrors pins that a failed call reaches the caller in
// the shared taxonomy, so the runtime's retry and circuit-breaker policies
// read a decision failure like any other provider failure.
func TestDecisionTransportErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		check  func(error) bool
	}{
		{"bad request", http.StatusBadRequest, errdefs.IsValidation},
		{"unauthorized", http.StatusUnauthorized, errdefs.IsUnauthorized},
		{"rate limit", http.StatusTooManyRequests, errdefs.IsRateLimit},
		{"server", http.StatusInternalServerError, errdefs.IsNotAvailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := newCapturedOpenAI(t, func(
				w http.ResponseWriter,
				_ *http.Request,
				_ map[string]any,
			) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(
					w, `{"error":{"message":"boom","type":"api_error","code":"x"}}`)
			})
			defer server.Close()
			cls := testClients(t, server)
			driver, err := openDecisionFor(
				cls,
				declarations["gpt-6-luna"],
				model.ModelID{Provider: "openai", Name: "gpt-6-luna"},
				"default",
			)
			if err != nil {
				t.Fatalf("openDecision: %v", err)
			}
			_, err = driver.Execute(
				context.Background(),
				openaiModel("gpt-6-luna"),
				decisionRequestFixture(),
			)
			if err == nil {
				t.Fatal("Execute succeeded, want a classified error")
			}
			if !inference.IsKind(err, inference.ProviderFailure) {
				t.Fatalf("error = %v, want ProviderFailure", err)
			}
			if !tc.check(err) {
				t.Fatalf("classified error = %v", err)
			}
		})
	}
}

// TestDecisionUsageWithoutTotal pins the counter mapping when the surface
// reports no total of its own: the canonical total is the sum of the halves,
// because a zero total would read downstream as a call that used no tokens.
func TestDecisionUsageWithoutTotal(t *testing.T) {
	server, _ := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		_ *http.Request,
		_ map[string]any,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{
			"model": "gpt-6-luna-2026-09-01",
			"answers": [{"type": "predicate", "name": "spam", "probability": 0.5}],
			"usage": {"input_tokens": 10, "output_tokens": 5}
		}`)
	})
	defer server.Close()
	cls := testClients(t, server)
	driver, err := openDecisionFor(
		cls,
		declarations["gpt-6-luna"],
		model.ModelID{Provider: "openai", Name: "gpt-6-luna"},
		"default",
	)
	if err != nil {
		t.Fatalf("openDecision: %v", err)
	}
	request := decisionRequestFixture()
	request.Questions = request.Questions[:1]
	response, err := driver.Execute(
		context.Background(),
		openaiModel("gpt-6-luna"),
		request,
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	usage := response.Usage
	if usage.InputTokens != 10 || usage.OutputTokens != 5 ||
		usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v, want the halves to add up", usage)
	}
}
