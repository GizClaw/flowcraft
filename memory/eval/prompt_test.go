package main

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory/eval/dataset"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/message"
)

func TestSystemPromptsLoaded(t *testing.T) {
	if strings.TrimSpace(answerSystem) == "" || strings.TrimSpace(judgeSystem) == "" {
		t.Fatal("system prompts must not be empty")
	}
}

func TestBuildAnswerInputRendersQuestionAndItems(t *testing.T) {
	content, err := buildAnswerInput(dataset.Question{
		Query:   "Where does the user live?",
		AskedAt: "2026-08-07",
	}, []sdkmemory.ContextItem{{
		Kind:        sdkmemory.ContextRawMessage,
		SourceClass: sdkmemory.ContextSourceRecent,
		Content: message.Content{Parts: []message.Part{
			message.TextPart{Text: "San Francisco"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	text := content.Text()
	if !strings.Contains(text, "Question: Where does the user live?") ||
		!strings.Contains(text, "The question was asked at: 2026-08-07") ||
		!strings.Contains(text, "[recent/raw_message]") ||
		!strings.Contains(text, "San Francisco") {
		t.Fatalf("answer input = %q", text)
	}
}

func TestBuildJudgeInputRendersGoldAndPrediction(t *testing.T) {
	content, err := buildJudgeInput([]string{"Paris"}, "The user lives in Paris.")
	if err != nil {
		t.Fatal(err)
	}
	text := content.Text()
	if !strings.Contains(text, "Gold:") || !strings.Contains(text, "- Paris") ||
		!strings.Contains(text, "Prediction:") || !strings.Contains(text, "The user lives in Paris.") {
		t.Fatalf("judge input = %q", text)
	}
}
