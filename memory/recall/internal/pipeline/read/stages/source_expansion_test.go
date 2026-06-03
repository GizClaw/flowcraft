package stages

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func TestSourceExpansionDoesNotRewriteUnanchoredQuery(t *testing.T) {
	plan := domain.QueryPlan{
		Intent:      domain.QueryIntent{Text: "What should I know about meeting people?"},
		TaskIntents: []domain.QueryTaskIntent{domain.QueryTaskDisambiguation},
	}

	got := sourceExpansionQueryTexts(plan)
	if len(got) != 1 || got[0] != plan.Intent.Text {
		t.Fatalf("unanchored query should not produce stopword-derived variants, got %v", got)
	}
}

func TestSourceExpansionRewritesAnchoredQuery(t *testing.T) {
	plan := domain.QueryPlan{
		Intent: domain.QueryIntent{
			Text: "What pets does Jordan have?",
			Features: domain.QueryFeatures{
				Proper: map[string]struct{}{"jordan": {}},
			},
		},
		TaskIntents: []domain.QueryTaskIntent{domain.QueryTaskSetCompletion},
	}

	got := sourceExpansionQueryTexts(plan)
	if len(got) < 2 {
		t.Fatalf("anchored query should keep compact variants available, got %v", got)
	}
}
