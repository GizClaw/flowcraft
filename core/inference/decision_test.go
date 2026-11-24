package inference_test

import (
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
)

// decisionRequest is a valid judgment call the tests narrow one field at a time.
func decisionRequest() inference.DecisionRequest {
	return inference.DecisionRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: "openai", Name: "gpt-4o-mini"}},
		Input: message.NewTextContent("the material being judged"),
		Questions: []inference.Question{
			{Name: "relevant", Kind: inference.QuestionPredicate, Instructions: "is it relevant?"},
		},
	}
}

func inferenceErrorKind(t *testing.T, err error) (inference.ErrorKind, inference.Field) {
	t.Helper()
	var typed *inference.Error
	if !errors.As(err, &typed) {
		t.Fatalf("err = %v, want an *inference.Error", err)
	}
	return typed.Kind, typed.Field
}

// TestDecisionRequestValidate is the gate before any provider I/O: a malformed
// judgment call must be refused here rather than answered by a model that
// guessed what was meant.
func TestDecisionRequestValidate(t *testing.T) {
	if err := decisionRequest().Validate(); err != nil {
		t.Fatalf("the fixture is invalid: %v", err)
	}

	boolTrue := true
	for name, tc := range map[string]struct {
		mutate func(*inference.DecisionRequest)
		kind   inference.ErrorKind
		field  inference.Field
	}{
		"no model": {
			mutate: func(r *inference.DecisionRequest) { r.Model = model.ModelRef{} },
			kind:   inference.InvalidRequest,
			field:  inference.FieldModel,
		},
		"no input": {
			mutate: func(r *inference.DecisionRequest) { r.Input = message.Content{} },
			kind:   inference.InvalidRequest,
			field:  inference.FieldInput,
		},
		"no questions": {
			mutate: func(r *inference.DecisionRequest) { r.Questions = nil },
			kind:   inference.InvalidRequest,
			field:  inference.FieldQuestions,
		},
		"question without a name": {
			mutate: func(r *inference.DecisionRequest) { r.Questions[0].Name = "" },
			kind:   inference.InvalidRequest,
			field:  inference.FieldQuestions,
		},
		"question without instructions": {
			mutate: func(r *inference.DecisionRequest) { r.Questions[0].Instructions = "" },
			kind:   inference.InvalidRequest,
			field:  inference.FieldQuestions,
		},
		"unknown kind": {
			mutate: func(r *inference.DecisionRequest) { r.Questions[0].Kind = "vibes" },
			kind:   inference.InvalidRequest,
			field:  inference.FieldQuestions,
		},
		"two questions with one name": {
			mutate: func(r *inference.DecisionRequest) {
				r.Questions = append(r.Questions, r.Questions[0])
			},
			kind:  inference.InvalidRequest,
			field: inference.FieldQuestions,
		},
		"choice question with one option": {
			mutate: func(r *inference.DecisionRequest) {
				r.Questions[0].Kind = inference.QuestionChoice
				r.Questions[0].Choices = []inference.ChoiceValue{{Label: "only"}}
			},
			kind:  inference.InvalidRequest,
			field: inference.FieldQuestions,
		},
		"score question without levels": {
			mutate: func(r *inference.DecisionRequest) {
				r.Questions[0].Kind = inference.QuestionScore
			},
			kind:  inference.InvalidRequest,
			field: inference.FieldQuestions,
		},
		"predicate question with choices": {
			mutate: func(r *inference.DecisionRequest) {
				r.Questions[0].Choices = []inference.ChoiceValue{{Label: "a", Bool: &boolTrue}, {Label: "b"}}
			},
			kind:  inference.InvalidRequest,
			field: inference.FieldQuestions,
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := decisionRequest()
			tc.mutate(&request)
			err := request.Validate()
			if err == nil {
				t.Fatal("Validate accepted the request")
			}
			kind, field := inferenceErrorKind(t, err)
			if kind != tc.kind || field != tc.field {
				t.Fatalf("got %s/%s, want %s/%s", kind, field, tc.kind, tc.field)
			}
		})
	}
}

func TestDecisionRequestActiveFields(t *testing.T) {
	fields := decisionRequest().ActiveFields()
	for _, want := range []inference.Field{
		inference.FieldModel, inference.FieldInput, inference.FieldQuestions,
	} {
		found := false
		for _, got := range fields {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("ActiveFields = %v, missing %q", fields, want)
		}
	}
}

// TestDecisionResponseValidateFor pins the rule that makes a judgment usable:
// answers are matched by name, and one that does not fit its question would
// otherwise be attributed silently.
func TestDecisionResponseValidateFor(t *testing.T) {
	request := decisionRequest()
	probability := 0.8
	badProbability := 1.5
	boolChoice := true

	valid := inference.DecisionResponse{
		Answers: []inference.Answer{{Name: "relevant", Kind: inference.AnswerKind(inference.QuestionPredicate), Probability: &probability}},
	}
	if err := valid.ValidateFor(request); err != nil {
		t.Fatalf("the valid response was refused: %v", err)
	}

	// A refusal is a valid answer: it is per question, and the others still count.
	refusal := inference.DecisionResponse{
		Answers: []inference.Answer{{Name: "relevant", Kind: inference.AnswerRefusal, Refusal: "cannot judge"}},
	}
	if err := refusal.ValidateFor(request); err != nil {
		t.Fatalf("a refusal was refused: %v", err)
	}

	choiceRequest := decisionRequest()
	choiceRequest.Questions[0].Kind = inference.QuestionChoice
	choiceRequest.Questions[0].Choices = []inference.ChoiceValue{
		{Label: "yes", Bool: &boolChoice},
		{Label: "no"},
	}

	for name, tc := range map[string]struct {
		request  inference.DecisionRequest
		response inference.DecisionResponse
	}{
		"too few answers": {
			request: request,
			response: inference.DecisionResponse{
				Answers: nil,
			},
		},
		"answer names no question": {
			request: request,
			response: inference.DecisionResponse{
				Answers: []inference.Answer{{Name: "other", Kind: inference.AnswerKind(inference.QuestionPredicate), Probability: &probability}},
			},
		},
		"kind does not match the question": {
			request: request,
			response: inference.DecisionResponse{
				Answers: []inference.Answer{{Name: "relevant", Kind: inference.AnswerKind(inference.QuestionScore), Score: &probability}},
			},
		},
		"probability out of range": {
			request: request,
			response: inference.DecisionResponse{
				Answers: []inference.Answer{{Name: "relevant", Kind: inference.AnswerKind(inference.QuestionPredicate), Probability: &badProbability}},
			},
		},
		"predicate without a probability": {
			request: request,
			response: inference.DecisionResponse{
				Answers: []inference.Answer{{Name: "relevant", Kind: inference.AnswerKind(inference.QuestionPredicate)}},
			},
		},
		"choice the question did not offer": {
			request: choiceRequest,
			response: inference.DecisionResponse{
				Answers: []inference.Answer{{
					Name: "relevant", Kind: inference.AnswerKind(inference.QuestionChoice),
					Choice: &inference.ChoiceValue{Label: "maybe"},
				}},
			},
		},
		"choice with the wrong typed value": {
			request: choiceRequest,
			response: inference.DecisionResponse{
				Answers: []inference.Answer{{
					Name: "relevant", Kind: inference.AnswerKind(inference.QuestionChoice),
					Choice: &inference.ChoiceValue{Label: "no", Bool: &boolChoice},
				}},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.response.ValidateFor(tc.request)
			if err == nil {
				t.Fatal("ValidateFor accepted the response")
			}
			kind, field := inferenceErrorKind(t, err)
			if kind != inference.InvalidProviderResponse || field != inference.FieldAnswers {
				t.Fatalf("got %s/%s, want %s/%s",
					kind, field, inference.InvalidProviderResponse, inference.FieldAnswers)
			}
		})
	}
}
