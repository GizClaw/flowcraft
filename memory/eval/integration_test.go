package main

import (
	"context"
	"os"
	"testing"
	"time"
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
	dataset := &Dataset{
		Name: "integration",
		Conversations: []Conversation{{
			ID: "c1",
			Turns: []Turn{
				{Role: "user", Content: "I moved to San Francisco last month.", EvidenceID: "c1:1"},
				{Role: "assistant", Content: "Welcome to SF!"},
			},
		}},
		Questions: []Question{{
			ID: "q1", ConversationID: "c1", Query: "Where does the user live now?",
			GoldAnswers: []string{"San Francisco"}, EvidenceIDs: []string{"c1:1"},
		}},
	}
	report, err := Run(context.Background(), runOptions{
		Dataset:       dataset,
		InferencePath: inferencePath,
		Suite:         "locomo",
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
