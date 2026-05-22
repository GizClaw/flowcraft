package asyncsemantic_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/internal/store/asyncsemantic"
	"github.com/GizClaw/flowcraft/memory/recall/recalltest"
)

func TestQueue_Conformance(t *testing.T) {
	recalltest.RunAsyncSemanticQueueSuite(t, func(testing.TB) recall.AsyncSemanticQueue {
		return asyncsemantic.New()
	})
}
