package openai

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

func decisionRequest() inference.DecisionRequest {
	flag := true
	return inference.DecisionRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "gpt-6-luna"}},
		Input: message.NewTextContent("the user asks to delete production data"),
		Questions: []inference.Question{
			{Name: "dangerous", Kind: inference.QuestionPredicate, Instructions: "is this destructive?"},
			{Name: "route", Kind: inference.QuestionChoice, Instructions: "who handles it?",
				Choices: []inference.ChoiceValue{{Label: "operator"}, {Label: "approve", Bool: &flag}}},
			{Name: "severity", Kind: inference.QuestionScore, Instructions: "how bad?",
				Levels: []string{"low", "mid", "high"}},
		},
	}
}

func decisionProvider(t *testing.T, body string) inference.Decider {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	provider, err := New(Settings{API: APIChat, APIKey: "test-key", BaseURL: server.URL})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	decider := provider.Decider
	if decider == nil {
		t.Fatal("provider does not expose Decider")
	}
	return decider
}

// Three question kinds in one call, answered in order, with a refusal that must
// not invalidate the other answers.
func TestDecisionDecodesThreeKinds(t *testing.T) {
	decider := decisionProvider(t, `{"model":"gpt-6-luna","usage":{"input_tokens":11,"output_tokens":3,"total_tokens":14},
		"answers":[
			{"name":"dangerous","type":"predicate","probability":0.97},
			{"name":"severity","type":"score","score":2,"confidence":0.8,
			 "probabilities":[{"label":"high","value":2,"probability":0.8}]},
			{"name":"route","type":"refusal"}]}`)

	response, err := decider.Decide(context.Background(), decisionRequest())
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if len(response.Answers) != 3 {
		t.Fatalf("answers = %d, want 3", len(response.Answers))
	}
	if response.Answers[0].Probability == nil || *response.Answers[0].Probability < 0.9 {
		t.Errorf("predicate answer = %+v", response.Answers[0])
	}
	if response.Answers[1].Score == nil || *response.Answers[1].Score != 2 {
		t.Errorf("score answer = %+v", response.Answers[1])
	}
	if response.Answers[2].Kind != inference.AnswerRefusal {
		t.Errorf("third answer = %+v, want a refusal", response.Answers[2])
	}
	if response.Usage.TotalTokens != 14 {
		t.Errorf("total tokens = %d, want 14", response.Usage.TotalTokens)
	}
}

// An answer whose kind does not match its question is a decoding error: it
// belongs to a different question, and attributing it would corrupt the result.
func TestDecisionRejectsKindMismatch(t *testing.T) {
	decider := decisionProvider(t, `{"model":"gpt-6-luna","usage":{},"answers":[
		{"name":"dangerous","type":"score","score":1},
		{"name":"route","type":"choice","choice":"operator","confidence":0.5},
		{"name":"severity","type":"score","score":2}]}`)

	_, err := decider.Decide(context.Background(), decisionRequest())
	if !inference.IsKind(err, inference.InvalidProviderResponse) {
		t.Errorf("kind = %v, want InvalidProviderResponse", err)
	}
}

// A choice the question never offered is a fabricated answer.
func TestDecisionRejectsUnknownChoice(t *testing.T) {
	decider := decisionProvider(t, `{"model":"gpt-6-luna","usage":{},"answers":[
		{"name":"dangerous","type":"predicate","probability":0.5},
		{"name":"route","type":"choice","choice":"nobody-offered-this","confidence":0.5},
		{"name":"severity","type":"score","score":1}]}`)

	_, err := decider.Decide(context.Background(), decisionRequest())
	if !inference.IsKind(err, inference.InvalidProviderResponse) {
		t.Errorf("kind = %v, want InvalidProviderResponse", err)
	}
}
