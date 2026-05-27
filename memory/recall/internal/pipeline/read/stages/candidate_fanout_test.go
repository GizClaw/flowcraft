package stages

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/domain/diagnostic"
	"github.com/GizClaw/flowcraft/memory/recall/internal/pipeline/read"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
)

type variantRecordingSource struct {
	name  string
	texts []string
}

func (s *variantRecordingSource) Name() string { return s.name }

func (s *variantRecordingSource) Query(_ context.Context, plan domain.QueryPlan) domain.SourceResult {
	s.texts = append(s.texts, plan.Intent.Text)
	switch plan.Intent.Text {
	case "What pets does Melanie have?":
		return domain.SourceResult{
			Source: s.name,
			Candidates: []domain.Candidate{
				{FactID: "bailey", Source: s.name, Rank: 1, Score: 0.90, EvidenceIDs: []string{"e1"}},
				{FactID: "shared", Source: s.name, Rank: 2, Score: 0.30, EvidenceIDs: []string{"e2"}},
			},
		}
	case "pets Melanie":
		return domain.SourceResult{
			Source: s.name,
			Candidates: []domain.Candidate{
				{FactID: "oliver", Source: s.name, Rank: 1, Score: 0.60, EvidenceIDs: []string{"e3"}},
				{FactID: "shared", Source: s.name, Rank: 2, Score: 0.70, EvidenceIDs: []string{"e4"}},
			},
		}
	default:
		return domain.SourceResult{Source: s.name}
	}
}

func TestCandidateFanoutUsesPlanDrivenQueryVariantsWithoutDuplicateBoost(t *testing.T) {
	src := &variantRecordingSource{name: "retrieval"}
	stage := NewCandidateFanout(func() []port.Source { return []port.Source{src} })
	intent := domain.QueryIntent{Text: "What pets does Melanie have?"}
	plan := domain.QueryPlan{
		Intent:        intent,
		SourceOrder:   []string{"retrieval"},
		SourceBudgets: map[string]int{"retrieval": 9},
		TotalCap:      9,
		TaskIntents:   []domain.QueryTaskIntent{domain.QueryTaskSetCompletion},
	}
	state := &read.ReadState{
		Scope:  domain.Scope{RuntimeID: "rt", UserID: "u"},
		Intent: &intent,
		Plan:   &plan,
	}

	detail, err := stage.Run(context.Background(), state)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(src.texts) < 2 {
		t.Fatalf("expected source to be queried with variants, got texts %+v", src.texts)
	}
	results := state.SubScopeStates[0].SourceResults
	if len(results) != 1 {
		t.Fatalf("source results = %+v", results)
	}
	seen := map[string]int{}
	for _, candidate := range results[0].Candidates {
		seen[candidate.FactID]++
	}
	if seen["shared"] != 1 {
		t.Fatalf("variant merge should dedupe repeated fact once, got candidates %+v", results[0].Candidates)
	}
	if len(results[0].Candidates) != 3 {
		t.Fatalf("variant merge should widen candidate pool without duplicates, got %+v", results[0].Candidates)
	}
	gotDetail := detail.(diagnostic.CandidateFanoutDetail)
	if len(gotDetail.Sources) != 1 || gotDetail.Sources[0].QueryVariants < 2 {
		t.Fatalf("source diagnostics should expose query variants, got %+v", gotDetail.Sources)
	}
}
