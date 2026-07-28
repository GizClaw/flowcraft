package locomo

import (
	"fmt"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/eval/locomo/runners/flowcraftv2"
	"github.com/GizClaw/flowcraft/memory/recall"
	"github.com/GizClaw/flowcraft/memory/recall/diagnostics"
	memoryretrieval "github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

// v2DiagnosticHooks bundles the v2 SaveDiagnostics / RecallDiagnostics
// callbacks the LoCoMo CLI threads into the v2 runner when the
// --diagnostics flag is set. Both fields are optional; the runner falls
// back to the cheaper non-explain path when nil.
type v2DiagnosticHooks struct {
	OnSave   func(runners.Scope, diagnostics.SaveDiagnostics)
	OnRecall func(runners.Scope, diagnostics.RecallDiagnostics)
}

const runnerFlowcraftRecallV2 = "flowcraft-recall-v2"

// normalizeRunnerName maps CLI aliases to the canonical temporal-recall runner.
func normalizeRunnerName(name string) (string, error) {
	switch name {
	case "flowcraft", "flowcraft-v2", runnerFlowcraftRecallV2:
		return runnerFlowcraftRecallV2, nil
	default:
		return "", fmt.Errorf("unknown runner: %s (want flowcraft-recall-v2)", name)
	}
}

type runnerConfig struct {
	LLM            llm.LLM
	RetrievalIndex memoryretrieval.Index
	ExtractorMode  recall.LLMExtractionMode
	Embedder       embedding.Embedder
	RerankerLLM    llm.LLM
}

func buildLocomoRunner(canonical string, cfg runnerConfig, onSaved func(runners.Scope, []string), onFacts func(runners.Scope, []recall.TemporalFact, *diagnostics.SaveDiagnostics), diag *v2DiagnosticHooks) (runners.Runner, error) {
	if canonical != runnerFlowcraftRecallV2 {
		return nil, fmt.Errorf("internal: unhandled runner %q", canonical)
	}
	opts := flowcraftv2.Options{
		Name:                 runnerFlowcraftRecallV2,
		LLM:                  cfg.LLM,
		RetrievalIndex:       cfg.RetrievalIndex,
		ExtractorMode:        cfg.ExtractorMode,
		Embedder:             cfg.Embedder,
		RerankerLLM:          cfg.RerankerLLM,
		IncludeAssistant:     true,
		OnFactsSaved:         onSaved,
		OnFactsSavedDetailed: onFacts,
	}
	if diag != nil {
		opts.OnSaveDiagnostics = diag.OnSave
		opts.OnRecallDiagnostics = diag.OnRecall
	}
	return flowcraftv2.New(opts)
}
