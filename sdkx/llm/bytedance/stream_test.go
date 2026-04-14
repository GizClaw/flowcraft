package bytedance

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestSortedToolCalls_NonContiguousIndices(t *testing.T) {
	s := &streamMessage{
		toolCalls: map[int]model.ToolCall{
			0: {ID: "tc0", Name: "search"},
			2: {ID: "tc2", Name: "code"},
			5: {ID: "tc5", Name: "read"},
		},
	}

	calls := s.sortedToolCalls()
	if len(calls) != 3 {
		t.Fatalf("expected 3 tool calls, got %d", len(calls))
	}
	if calls[0].ID != "tc0" {
		t.Fatalf("expected tc0 first, got %s", calls[0].ID)
	}
	if calls[1].ID != "tc2" {
		t.Fatalf("expected tc2 second, got %s", calls[1].ID)
	}
	if calls[2].ID != "tc5" {
		t.Fatalf("expected tc5 third, got %s", calls[2].ID)
	}
}

func TestSortedToolCalls_ContiguousIndices(t *testing.T) {
	s := &streamMessage{
		toolCalls: map[int]model.ToolCall{
			0: {ID: "tc0", Name: "a"},
			1: {ID: "tc1", Name: "b"},
			2: {ID: "tc2", Name: "c"},
		},
	}

	calls := s.sortedToolCalls()
	if len(calls) != 3 {
		t.Fatalf("expected 3, got %d", len(calls))
	}
	for i, tc := range calls {
		if tc.Name != string(rune('a'+i)) {
			t.Fatalf("expected %c at index %d, got %s", rune('a'+i), i, tc.Name)
		}
	}
}

func TestSortedToolCalls_Empty(t *testing.T) {
	s := &streamMessage{}
	if calls := s.sortedToolCalls(); calls != nil {
		t.Fatalf("expected nil, got %v", calls)
	}
}

func TestSortedToolCalls_SingleGap(t *testing.T) {
	s := &streamMessage{
		toolCalls: map[int]model.ToolCall{
			3: {ID: "tc3", Name: "only"},
		},
	}

	calls := s.sortedToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1, got %d", len(calls))
	}
	if calls[0].ID != "tc3" {
		t.Fatalf("expected tc3, got %s", calls[0].ID)
	}
}
