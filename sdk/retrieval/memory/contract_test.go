package memory_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
	"github.com/GizClaw/flowcraft/sdk/retrieval/contract"
	"github.com/GizClaw/flowcraft/sdk/retrieval/memory"
)

func TestContract(t *testing.T) {
	contract.Run(t, func(_ *testing.T) (retrieval.Index, func()) {
		return memory.New(), func() {}
	})
}
