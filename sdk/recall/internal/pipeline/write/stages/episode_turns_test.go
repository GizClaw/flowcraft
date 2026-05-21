package stages_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/pipeline/write/stages"
)

func TestParseCanonicalTurns_RoundTrip(t *testing.T) {
	content := "Alice: hello\nBob: world"
	got := stages.ParseCanonicalTurns(content)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Speaker != "Alice" || got[0].Text != "hello" {
		t.Errorf("turn0 = %+v", got[0])
	}
	if got[1].Speaker != "Bob" || got[1].Text != "world" {
		t.Errorf("turn1 = %+v", got[1])
	}
}
