package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/eval/dataset"
	"github.com/GizClaw/flowcraft/memory/eval/scenarios"
	"github.com/GizClaw/flowcraft/sdk/message"
)

// TestRunIntegration exercises the full real-inference path. It self-skips
// unless MEMORY_EVAL_INFERENCE points at an inference.yaml with bytedance
// credentials, so plain `make eval` / `go test ./eval` stays offline.
func TestRunIntegration(t *testing.T) {
	inferencePath := os.Getenv("MEMORY_EVAL_INFERENCE")
	if inferencePath == "" {
		t.Skip("MEMORY_EVAL_INFERENCE is not set; skipping credentialed eval integration")
	}
	generateModel := envOrDefault("MEMORY_EVAL_GENERATE_MODEL", "bytedance:doubao-seed-2-0-lite")
	embedModel := envOrDefault("MEMORY_EVAL_EMBED_MODEL", "bytedance:doubao-embedding-vision")
	scenario, err := scenarios.Lookup("locomo")
	if err != nil {
		t.Fatal(err)
	}
	ds := &dataset.Dataset{
		Name: "integration",
		Conversations: []dataset.Conversation{{
			ID: "c1",
			Turns: []dataset.Turn{
				{
					Message: message.Message{
						Role:    message.RoleUser,
						Content: message.Content{Parts: []message.Part{message.TextPart{Text: "I moved to San Francisco last month."}}},
					},
					EvidenceID: "c1:1",
				},
				{
					Message: message.Message{
						Role:    message.RoleAssistant,
						Content: message.Content{Parts: []message.Part{message.TextPart{Text: "Welcome to SF!"}}},
					},
				},
			},
		}},
		Questions: []dataset.Question{{
			ID: "q1", ConversationID: "c1", Query: "Where does the user live now?",
			GoldAnswers: []string{"San Francisco"}, EvidenceIDs: []string{"c1:1"},
		}},
	}
	report, err := Run(context.Background(), runOptions{
		Dataset:       ds,
		InferencePath: inferencePath,
		Scenario:      scenario,
		GenerateModel: generateModel,
		EmbedModel:    embedModel,
		AnswerModel:   generateModel,
		JudgeModel:    "",
		MaxItems:      10,
		MaxTokens:     2048,
		Concurrency:   1,
		QATimeout:     2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.PerQuestion) != 1 {
		t.Fatalf("per-question = %d", len(report.PerQuestion))
	}
	if report.PolicyDigest == "" {
		t.Fatal("policy digest is empty")
	}
}

func envOrDefault(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
