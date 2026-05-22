package recall_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall"
	"github.com/GizClaw/flowcraft/sdk/recall/recalltest"
)

func TestInMemoryTemporalStore_Conformance(t *testing.T) {
	recalltest.RunTemporalStoreSuite(t, func(testing.TB) recall.TemporalStore {
		return recall.NewInMemoryTemporalStore()
	})
}

func TestInMemorySideEffectOutbox_Conformance(t *testing.T) {
	recalltest.RunSideEffectOutboxSuite(t, func(testing.TB) recall.SideEffectOutbox {
		return recall.NewInMemorySideEffectOutbox()
	})
}

func TestInMemoryAsyncSemanticQueue_Conformance(t *testing.T) {
	recalltest.RunAsyncSemanticQueueSuite(t, func(testing.TB) recall.AsyncSemanticQueue {
		return recall.NewInMemoryAsyncSemanticQueue()
	})
}
