package middleware

import (
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/core/tool"
)

func TestDefaultChainShape(t *testing.T) {
	chain, err := FromSettings(Settings{})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}
	want := []tool.Middleware{Recover()}
	partLimiter, _ := ResultPartLimiter(DefaultResultPartBudget)
	want = append(want, partLimiter)
	if len(chain) != len(want) {
		t.Fatalf("default chain = %d entries, want %d", len(chain), len(want))
	}
	for i := range want {
		if reflect.ValueOf(chain[i]).Pointer() != reflect.ValueOf(want[i]).Pointer() {
			t.Fatalf("default entry %d mismatch", i)
		}
	}
}
