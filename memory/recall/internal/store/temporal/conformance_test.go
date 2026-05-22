package temporal_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/internal/store/temporal"
	"github.com/GizClaw/flowcraft/memory/recall/recalltest"
)

func TestMemoryStore_Conformance(t *testing.T) {
	recalltest.RunTemporalStoreSuite(t, func(testing.TB) recall.TemporalStore {
		return temporal.NewMemoryStore()
	})
}
