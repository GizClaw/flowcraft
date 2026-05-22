package recall

import (
	"context"
	"testing"
)

func drainSideEffectsForTest(t testing.TB, mem Memory, scope Scope) SideEffectProcessResult {
	t.Helper()
	proc, ok := NewSideEffectProcessor(mem)
	if !ok {
		t.Fatal("side-effect processor missing")
	}
	var total SideEffectProcessResult
	for i := 0; i < 32; i++ {
		out, err := proc.ProcessSideEffects(context.Background(), SideEffectProcessOptions{
			Scope: scope,
			Limit: 128,
		})
		if err != nil {
			t.Fatalf("ProcessSideEffects: %v", err)
		}
		total.Claimed += out.Claimed
		total.Completed += out.Completed
		total.Failed += out.Failed
		total.DeadLetter += out.DeadLetter
		if out.Claimed == 0 || out.Failed > 0 {
			return total
		}
	}
	t.Fatal("side-effect processor did not quiesce")
	return total
}
