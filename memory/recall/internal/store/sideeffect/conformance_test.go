package sideeffect_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/internal/store/sideeffect"
	"github.com/GizClaw/flowcraft/memory/recall/recalltest"
)

func TestQueue_Conformance(t *testing.T) {
	recalltest.RunSideEffectOutboxSuite(t, func(testing.TB) recall.SideEffectOutbox {
		return sideeffect.New()
	})
}
