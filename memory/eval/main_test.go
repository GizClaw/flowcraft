package main

import (
	"context"
	"strings"
	"testing"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

func TestCLIRejectsMissingRunFlags(t *testing.T) {
	err := runCLI(context.Background(), []string{"run"})
	if err == nil || !strings.Contains(err.Error(), "--dataset") {
		t.Fatalf("err = %v", err)
	}
}

func TestComputeKHit(t *testing.T) {
	evidence := map[uint64]bool{2: true, 5: true}
	items := []sdkmemory.ContextItem{
		{Kind: sdkmemory.ContextRawMessage, Sequence: 1},
		{Kind: sdkmemory.ContextRawMessage, Sequence: 2},
		{
			Kind: sdkmemory.ContextFact,
			Sources: []sdkmemory.SourceRef{{
				Kind: sdkmemory.SourceMessage, ID: "c/msg-00000000000000000005", Revision: "5",
			}},
		},
		{Kind: sdkmemory.ContextRawMessage, Sequence: 9},
	}
	hit := computeKHit(items, evidence)
	if !hit.Hit || !hit.Message || !hit.Fact {
		t.Fatalf("k_hit = %+v", hit)
	}
}
