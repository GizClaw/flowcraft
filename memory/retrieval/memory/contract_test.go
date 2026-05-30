package memory_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/retrieval/contract"
	"github.com/GizClaw/flowcraft/memory/retrieval/memory"
)

func TestContract(t *testing.T) {
	contract.Run(t, func(_ *testing.T) (retrieval.Index, func()) {
		return memory.New(), func() {}
	})
}
