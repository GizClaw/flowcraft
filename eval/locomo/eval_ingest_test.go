package locomo

import (
	"testing"

	"github.com/GizClaw/flowcraft/eval/dataset"
)

func TestBatchTurnsByOnlineSavePointGroupsAdjacentExchange(t *testing.T) {
	batches := batchTurnsByOnlineSavePoint(dataset.Conversation{
		ID: "conv-1",
		Turns: []dataset.Turn{
			{Role: "user", Content: "A1", Speaker: "Alice", Timestamp: "t1", EvidenceID: "D1:1", SessionID: "session_1"},
			{Role: "assistant", Content: "B1", Speaker: "Bob", Timestamp: "t1", EvidenceID: "D1:2", SessionID: "session_1"},
			{Role: "user", Content: "A2", Speaker: "Alice", Timestamp: "t2", EvidenceID: "D1:3", SessionID: "session_1"},
			{Role: "assistant", Content: "B2", EvidenceID: "D1:4", SessionID: "session_1"},
			{Role: "user", Content: "A3", EvidenceID: "D2:1", SessionID: "session_2"},
		},
	})

	if len(batches) != 3 {
		t.Fatalf("batch count = %d, want 3", len(batches))
	}
	if got := len(batches[0].rawTurns); got != 2 {
		t.Fatalf("first batch raw turns = %d, want 2", got)
	}
	if batches[0].rawTurns[0].EvidenceID != "D1:1" || batches[0].rawTurns[1].EvidenceID != "D1:2" {
		t.Fatalf("first batch should contain the first adjacent exchange: %+v", batches[0].rawTurns)
	}
	if batches[0].rawTurns[0].Speaker != "Alice" || batches[0].rawTurns[0].Timestamp != "t1" {
		t.Fatalf("first batch should preserve structured speaker/time: %+v", batches[0].rawTurns[0])
	}
	if got := len(batches[1].recentRawTurns); got != 2 {
		t.Fatalf("second batch recent turns = %d, want 2", got)
	}
	if batches[1].recentRawTurns[1].Speaker != "Bob" || batches[1].recentRawTurns[1].Timestamp != "t1" {
		t.Fatalf("recent context should preserve structured speaker/time: %+v", batches[1].recentRawTurns[1])
	}
	if batches[1].rawTurns[0].EvidenceID != "D1:3" || batches[1].rawTurns[1].EvidenceID != "D1:4" {
		t.Fatalf("second batch should contain the second adjacent exchange: %+v", batches[1].rawTurns)
	}
	if got := len(batches[2].recentRawTurns); got != 0 {
		t.Fatalf("new session should not inherit recent turns, got %d", got)
	}
}
