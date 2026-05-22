package sideeffect_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/store/sideeffect"
	"github.com/GizClaw/flowcraft/sdk/recall/recalltest"
)

func TestQueue_Conformance(t *testing.T) {
	recalltest.RunSideEffectOutboxSuite(t, func(testing.TB) recall.SideEffectOutbox {
		return sideeffect.New()
	})
}
