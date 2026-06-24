package tasks

import (
	"context"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/eval/locomo/dataset"
	locomoreport "github.com/GizClaw/flowcraft/eval/locomo/report"
	"github.com/GizClaw/flowcraft/memory"
	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/llm"
)

const (
	DefaultQATopK                   = 12
	DefaultQARecentWindowMessages   = 0
	DefaultQAGraphExpandedMaxSource = 0

	qaSummaryRefsPerNode       = 3
	qaSummaryExpandedMaxSource = 18
	qaSummaryNeighborBefore    = 1
	qaSummaryNeighborAfter     = 1
	qaSummaryNeighborMaxSource = 4
	qaEntityRefsPerFact        = 3
	qaEntityExpandedMaxSource  = 4
	qaGraphMaxSeedFacts        = 8
	qaGraphMaxFactsPerSeed     = 8
	qaGraphMaxBridgeFacts      = 8
	qaGraphRefsPerPath         = 2

	qaEntityDirectBoostWeight       = 0.35
	qaEntitySupplementMinConfidence = 0.5
	qaEntitySupplementMinRelative   = 0.5

	qaRetrievalOriginMetadataKey = "retrieval_origin"
	qaRetrievalOriginDirect      = "source_direct"
	qaRetrievalOriginSummary     = "summary_expanded"
	qaRetrievalOriginNeighbor    = "source_neighborhood_expanded"
	qaRetrievalOriginEntity      = "entity_fact_expanded"
	qaRetrievalOriginGraph       = "graph_fact_expanded"

	qaGraphOriginMetadataKey  = "graph_origin"
	qaGraphFactIDsMetadataKey = "graph_fact_ids"
	qaGraphPathMetadataKey    = "graph_path"
	qaGraphScoreMetadataKey   = "graph_score"
	qaGraphSeedIDsMetadataKey = "graph_seed_entity_ids"
)

type QARetrievalOptions struct {
	TopK                   int
	GraphExpandedMaxSource int
}

func RunQA(ctx context.Context, mem *memory.System, answer llm.LLM, judge llm.LLM, scope memory.Scope, item dataset.QAItem, qaTopK int, timeout time.Duration) locomoreport.QAResult {
	return RunQAWithOptions(ctx, mem, answer, judge, scope, item, QARetrievalOptions{TopK: qaTopK}, timeout)
}

func RunQAWithOptions(ctx context.Context, mem *memory.System, answer llm.LLM, judge llm.LLM, scope memory.Scope, item dataset.QAItem, opts QARetrievalOptions, timeout time.Duration) locomoreport.QAResult {
	row := locomoreport.QAResult{ID: item.ID, Category: item.Category, Question: item.Question, Gold: item.Answer}
	pack, err := RetrieveQAContextForDiagnosticsWithOptions(ctx, mem, scope, item, opts)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	row.HitCounts = contextHitCounts(pack)
	row.EvidenceRecall = evidenceRecall(observedDiaIDs(pack), item.Evidence)
	if answer == nil {
		return row
	}
	pred, err := generateTextMessages(ctx, answer, qaAnswerMessages(item, pack), timeout)
	if err != nil {
		row.Error = err.Error()
		return row
	}
	row.Predicted = pred
	row.F1 = scoreQA(item, pred)
	if judge != nil {
		row.Judge = judgeQA(ctx, judge, item, pred, timeout)
	}
	return row
}

// RetrieveQAContextForDiagnostics returns the exact QA context pack used by RunQA.
// It is intended for eval analysis commands that need runtime-aligned retrieval metrics.
func RetrieveQAContextForDiagnostics(ctx context.Context, mem *memory.System, scope memory.Scope, item dataset.QAItem, qaTopK int) (*memory.ContextPack, error) {
	return RetrieveQAContextForDiagnosticsWithOptions(ctx, mem, scope, item, QARetrievalOptions{TopK: qaTopK})
}

func RetrieveQAContextForDiagnosticsWithOptions(ctx context.Context, mem *memory.System, scope memory.Scope, item dataset.QAItem, opts QARetrievalOptions) (*memory.ContextPack, error) {
	qaTopK := opts.TopK
	if qaTopK <= 0 {
		qaTopK = DefaultQATopK
	}
	searchTopK := qaTopK + qaSummaryExpandedMaxSource + qaEntityExpandedMaxSource
	return mem.PackContext(ctx, memory.ContextRequest{
		Scope: scope,
		Query: strings.TrimSpace(item.Question),
		TopK:  searchTopK,
		Window: recent.WindowRequest{Budget: &recent.WindowBudget{
			MaxMessages: DefaultQARecentWindowMessages,
		}},
		PackOptions: derive.ContextPackOptions{
			SourceEvidence: derive.SourceEvidencePackOptions{
				MaxDirectMessages: qaTopK,
				MaxGraphMessages:  opts.GraphExpandedMaxSource,
			},
		},
	})
}
