package main

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/eval/dataset"
	"github.com/GizClaw/flowcraft/sdk/message"
)

func turn(role message.Role, text, session string) dataset.Turn {
	return dataset.Turn{
		Message: message.Message{
			Role:    role,
			Content: message.Content{Parts: []message.Part{message.TextPart{Text: text}}},
		},
		SessionID: session,
	}
}

func rolesOf(batches [][]dataset.Turn) [][]string {
	var result [][]string
	for _, batch := range batches {
		var roles []string
		for _, turn := range batch {
			roles = append(roles, string(turn.Message.Role))
		}
		result = append(result, roles)
	}
	return result
}

func TestBatchTurnsSession(t *testing.T) {
	turns := []dataset.Turn{
		turn(message.RoleUser, "u1", "s1"),
		turn(message.RoleAssistant, "a1", "s1"),
		turn(message.RoleUser, "u2", "s1"),
		turn(message.RoleAssistant, "a2", "s1"),
	}
	batches := batchTurns(turns, granularitySession)
	if len(batches) != 1 || len(batches[0]) != 4 {
		t.Fatalf("session granularity = %#v", rolesOf(batches))
	}
}

func TestBatchTurnsExchange(t *testing.T) {
	turns := []dataset.Turn{
		turn(message.RoleUser, "u1", "s1"),
		turn(message.RoleAssistant, "a1", "s1"),
		turn(message.RoleUser, "u2", "s1"),
		turn(message.RoleAssistant, "a2", "s1"),
		turn(message.RoleUser, "u3", "s1"),
		turn(message.RoleUser, "u3b", "s1"),
		turn(message.RoleAssistant, "a3", "s1"),
	}
	batches := batchTurns(turns, granularityExchange)
	got := rolesOf(batches)
	want := [][]string{{"user", "assistant"}, {"user", "assistant"}, {"user", "user", "assistant"}}
	if len(got) != len(want) {
		t.Fatalf("exchange granularity = %#v, want %#v", got, want)
	}
	for index := range want {
		if len(got[index]) != len(want[index]) {
			t.Fatalf("exchange granularity = %#v, want %#v", got, want)
		}
		for i := range want[index] {
			if got[index][i] != want[index][i] {
				t.Fatalf("exchange granularity = %#v, want %#v", got, want)
			}
		}
	}
}

func TestBatchTurnsTurnAndSessionBoundary(t *testing.T) {
	turns := []dataset.Turn{
		turn(message.RoleUser, "u1", "s1"),
		turn(message.RoleAssistant, "a1", "s1"),
		turn(message.RoleUser, "u2", "s2"),
	}
	batches := batchTurns(turns, granularityTurn)
	if len(batches) != 3 {
		t.Fatalf("turn granularity = %#v", rolesOf(batches))
	}
	batches = batchTurns(turns, granularitySession)
	if len(batches) != 2 {
		t.Fatalf("session boundary = %#v", rolesOf(batches))
	}
}
