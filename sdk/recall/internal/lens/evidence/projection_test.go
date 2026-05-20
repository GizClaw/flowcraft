package evidence

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/projection"
)

func TestProjection_ConsistencyIsRequired(t *testing.T) {
	p := New(nil)
	if got := p.Consistency(); got != projection.Required {
		t.Fatalf("Consistency = %v, want Required", got)
	}
}
