package ingest

import (
	"context"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

type extractorUsageContextKey struct{}

type extractorUsageAccumulator struct {
	mu     sync.Mutex
	total  diagnostic.ExtractorTokenUsage
	stages map[string]*diagnostic.ExtractorStageTokenUsage
}

func newExtractorUsageAccumulator() *extractorUsageAccumulator {
	return &extractorUsageAccumulator{stages: map[string]*diagnostic.ExtractorStageTokenUsage{}}
}

func withExtractorUsageAccumulator(ctx context.Context, acc *extractorUsageAccumulator) context.Context {
	if acc == nil {
		return ctx
	}
	return context.WithValue(ctx, extractorUsageContextKey{}, acc)
}

func recordExtractorTokenUsage(ctx context.Context, stage string, usage llm.TokenUsage) {
	acc, _ := ctx.Value(extractorUsageContextKey{}).(*extractorUsageAccumulator)
	if acc == nil {
		return
	}
	acc.add(stage, tokenUsageFromLLM(usage))
}

func (a *extractorUsageAccumulator) add(stage string, usage diagnostic.TokenUsage) {
	if a == nil {
		return
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.total.Calls++
	addTokenUsage(&a.total.TokenUsage, usage)
	if stage == "" {
		stage = "unknown"
	}
	st, ok := a.stages[stage]
	if !ok {
		st = &diagnostic.ExtractorStageTokenUsage{Stage: stage}
		a.stages[stage] = st
	}
	st.Calls++
	addTokenUsage(&st.TokenUsage, usage)
}

func (a *extractorUsageAccumulator) snapshot() diagnostic.ExtractorTokenUsage {
	if a == nil {
		return diagnostic.ExtractorTokenUsage{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.total
	fillExtractorAverages(&out)
	keys := make([]string, 0, len(a.stages))
	for key := range a.stages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out.Stages = make([]diagnostic.ExtractorStageTokenUsage, 0, len(keys))
	for _, key := range keys {
		st := *a.stages[key]
		fillStageAverages(&st)
		out.Stages = append(out.Stages, st)
	}
	return out
}

func tokenUsageFromLLM(usage llm.TokenUsage) diagnostic.TokenUsage {
	return diagnostic.TokenUsage{
		InputTokens:       usage.InputTokens,
		CachedInputTokens: usage.CachedInputTokens,
		OutputTokens:      usage.OutputTokens,
		TotalTokens:       usage.TotalTokens,
		Model:             usage.Model,
		CostMicros:        usage.CostMicros,
	}
}

func addTokenUsage(dst *diagnostic.TokenUsage, usage diagnostic.TokenUsage) {
	dst.InputTokens += usage.InputTokens
	dst.CachedInputTokens += usage.CachedInputTokens
	dst.OutputTokens += usage.OutputTokens
	dst.TotalTokens += usage.TotalTokens
	dst.CostMicros += usage.CostMicros
	if dst.Model == "" {
		dst.Model = usage.Model
	}
}

func fillExtractorAverages(usage *diagnostic.ExtractorTokenUsage) {
	if usage == nil || usage.Calls <= 0 {
		return
	}
	calls := float64(usage.Calls)
	usage.AvgInputTokensPerCall = float64(usage.InputTokens) / calls
	usage.AvgOutputTokensPerCall = float64(usage.OutputTokens) / calls
	usage.AvgTotalTokensPerCall = float64(usage.TotalTokens) / calls
}

func fillStageAverages(usage *diagnostic.ExtractorStageTokenUsage) {
	if usage == nil || usage.Calls <= 0 {
		return
	}
	calls := float64(usage.Calls)
	usage.AvgInputTokensPerCall = float64(usage.InputTokens) / calls
	usage.AvgOutputTokensPerCall = float64(usage.OutputTokens) / calls
	usage.AvgTotalTokensPerCall = float64(usage.TotalTokens) / calls
}
