package evidence

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

func TestProjection_ConsistencyIsRequired(t *testing.T) {
	p := New(nil)
	if got := p.Consistency(); got != port.Required {
		t.Fatalf("Consistency = %v, want Required", got)
	}
}
