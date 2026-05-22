package evidence_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/evidence"
	"github.com/GizClaw/flowcraft/sdk/recall/recalltest"
)

func TestMemoryStore_Conformance(t *testing.T) {
	recalltest.RunEvidenceStoreSuite(t, func(testing.TB) recall.EvidenceStore {
		return evidence.NewMemoryStore()
	})
}
