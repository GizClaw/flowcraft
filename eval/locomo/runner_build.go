package locomo

import (
	"fmt"

	"github.com/GizClaw/flowcraft/eval/locomo/runners"
	"github.com/GizClaw/flowcraft/eval/locomo/runners/flowcraft"
	"github.com/GizClaw/flowcraft/eval/locomo/runners/flowcraftv2"
	"github.com/GizClaw/flowcraft/sdk/embedding"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall"
	recallv1 "github.com/GizClaw/flowcraft/sdk/recall_v1"
)

// v2DiagnosticHooks bundles the v2 SaveDiagnostics / RecallDiagnostics
// callbacks the LoCoMo CLI threads into the v2 runner when the
// --diagnostics flag is set. Both fields are optional; the runner falls
// back to the cheaper non-explain path when nil.
type v2DiagnosticHooks struct {
	OnSave   func(runners.Scope, recall.SaveDiagnostics)
	OnRecall func(runners.Scope, recall.RecallDiagnostics)
}

// normalizeRunnerName maps legacy CLI aliases to canonical runner names.
func normalizeRunnerName(name string) (string, error) {
	switch name {
	case "flowcraft":
		return "flowcraft-v1", nil
	case "flowcraft-v1", "flowcraft-v2":
		return name, nil
	default:
		return "", fmt.Errorf("unknown runner: %s (want flowcraft, flowcraft-v1, or flowcraft-v2)", name)
	}
}

// v1RunnerConfig carries knobs for the recall_v1 flowcraft runner only.
type v1RunnerConfig struct {
	Name                      string
	LLM                       llm.LLM
	Embedder                  embedding.Embedder
	MaxFactsPerCall           int
	IncludeAssistant          bool
	SaveWithContext           bool
	SoftMerge                 *bool
	RerankerLLM               llm.LLM
	ScoreThreshold            float64
	MultiRecall               bool
	EntityStore               bool
	EntityStoreMaxLinkedCount int
	EntityLinkBoost           float64
	QueryEntityLLM            llm.LLM
	UpdateResolverLLM         llm.LLM
	RecentTurnsK              int
	OnFactsExtracted          func(recallv1.Scope, []recallv1.ExtractedFact)
}

func buildLocomoRunner(canonical string, v1 v1RunnerConfig, v2OnSaved func(runners.Scope, []string), v2Diag *v2DiagnosticHooks) (runners.Runner, error) {
	switch canonical {
	case "flowcraft-v2":
		opts := flowcraftv2.Options{
			Name:             "flowcraft-v2",
			LLM:              v1.LLM,
			Embedder:         v1.Embedder,
			RerankerLLM:      v1.RerankerLLM,
			IncludeAssistant: true,
			OnFactsSaved:     v2OnSaved,
		}
		if v2Diag != nil {
			opts.OnSaveDiagnostics = v2Diag.OnSave
			opts.OnRecallDiagnostics = v2Diag.OnRecall
		}
		return flowcraftv2.New(opts)
	case "flowcraft-v1":
		v1.Name = canonical
		return flowcraft.New(flowcraft.Options{
			Name:                      v1.Name,
			LLM:                       v1.LLM,
			Embedder:                  v1.Embedder,
			MaxFactsPerCall:           v1.MaxFactsPerCall,
			IncludeAssistant:          v1.IncludeAssistant,
			SaveWithContext:           v1.SaveWithContext,
			SoftMerge:                 v1.SoftMerge,
			RerankerLLM:               v1.RerankerLLM,
			ScoreThreshold:            v1.ScoreThreshold,
			MultiRecall:               v1.MultiRecall,
			EntityStore:               v1.EntityStore,
			EntityStoreMaxLinkedCount: v1.EntityStoreMaxLinkedCount,
			EntityLinkBoost:           v1.EntityLinkBoost,
			QueryEntityLLM:            v1.QueryEntityLLM,
			UpdateResolverLLM:         v1.UpdateResolverLLM,
			RecentTurnsK:              v1.RecentTurnsK,
			OnFactsExtracted:          v1.OnFactsExtracted,
		})
	default:
		return nil, fmt.Errorf("internal: unhandled runner %q", canonical)
	}
}
