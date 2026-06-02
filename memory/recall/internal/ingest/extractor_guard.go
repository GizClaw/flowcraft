package ingest

import (
	"context"
	"sync"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
)

type extractorGuardContextKey struct{}

type extractorGuardAccumulator struct {
	mu  sync.Mutex
	out diagnostic.ExtractorGuard
}

func newExtractorGuardAccumulator() *extractorGuardAccumulator {
	return &extractorGuardAccumulator{}
}

func withExtractorGuardAccumulator(ctx context.Context, acc *extractorGuardAccumulator) context.Context {
	if acc == nil {
		return ctx
	}
	return context.WithValue(ctx, extractorGuardContextKey{}, acc)
}

func recordExtractorCandidateAccepted(ctx context.Context) {
	acc, _ := ctx.Value(extractorGuardContextKey{}).(*extractorGuardAccumulator)
	if acc == nil {
		return
	}
	acc.addAccepted()
}

func recordExtractorCandidateRejected(ctx context.Context, fact diagnostic.GuardedExtractedFact) {
	acc, _ := ctx.Value(extractorGuardContextKey{}).(*extractorGuardAccumulator)
	if acc == nil {
		return
	}
	acc.addRejected(fact)
}

func (a *extractorGuardAccumulator) addAccepted() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.out.Candidates++
	a.out.Accepted++
}

func (a *extractorGuardAccumulator) addRejected(fact diagnostic.GuardedExtractedFact) {
	if a == nil {
		return
	}
	if fact.GuardReason == "" {
		fact.GuardReason = "unknown"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.out.Candidates++
	a.out.Rejected++
	if a.out.ByReason == nil {
		a.out.ByReason = map[string]int{}
	}
	a.out.ByReason[fact.GuardReason]++
	a.out.RejectedFacts = append(a.out.RejectedFacts, fact)
}

func (a *extractorGuardAccumulator) snapshot() diagnostic.ExtractorGuard {
	if a == nil {
		return diagnostic.ExtractorGuard{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.out
	if len(a.out.ByReason) > 0 {
		out.ByReason = make(map[string]int, len(a.out.ByReason))
		for k, v := range a.out.ByReason {
			out.ByReason[k] = v
		}
	}
	if len(a.out.RejectedFacts) > 0 {
		out.RejectedFacts = append([]diagnostic.GuardedExtractedFact(nil), a.out.RejectedFacts...)
	}
	return out
}
