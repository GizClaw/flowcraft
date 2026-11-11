package retrieval

import (
	"strings"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

func TestClampBudgetBoundsRequestsAndKeepsUnset(t *testing.T) {
	huge := ClampBudget(corememory.Budget{MaxItems: 1 << 20, MaxTokens: 1 << 30, MaxChars: 1 << 30})
	if huge.MaxItems != MaxContextItems || huge.MaxTokens != MaxContextTokens || huge.MaxChars != MaxContextChars {
		t.Fatalf("clamped budget = %+v", huge)
	}
	if unset := ClampBudget(corememory.Budget{}); unset != (corememory.Budget{}) {
		t.Fatalf("unset budget changed: %+v", unset)
	}
	within := ClampBudget(corememory.Budget{MaxItems: 4, MaxTokens: 100, MaxChars: 200})
	if within != (corememory.Budget{MaxItems: 4, MaxTokens: 100, MaxChars: 200}) {
		t.Fatalf("within-limit budget changed: %+v", within)
	}
}

func TestTruncateItemContentBoundsTextAndKeepsParts(t *testing.T) {
	item := corememory.ContextItem{
		ID: "item", Kind: corememory.ContextFact,
		Content: coremessage.Content{Parts: []coremessage.Part{
			coremessage.TextPart{Text: strings.Repeat("x", 4000)},
			coremessage.DataPart{MediaType: "application/json", Value: []byte(`{"k":"v"}`)},
			coremessage.TextPart{Text: strings.Repeat("y", 4000)},
		}},
		Sources: []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "source"}},
	}
	bounded, cut := TruncateItemContent(item, 10, 0)
	if !cut || bounded.TokenCount > 10 {
		t.Fatalf("bounded = tokens %d cut %v, want cut within 10 tokens", bounded.TokenCount, cut)
	}
	if len(bounded.Content.Parts) != 3 {
		t.Fatalf("parts = %d, want non-text parts preserved", len(bounded.Content.Parts))
	}
	if _, ok := bounded.Content.Parts[1].(coremessage.DataPart); !ok {
		t.Fatalf("part 1 = %T, want DataPart", bounded.Content.Parts[1])
	}
	if original := item.Content.Parts[0].(coremessage.TextPart).Text; len(original) != 4000 {
		t.Fatal("truncation must not mutate the caller's content")
	}
	if _, cut := TruncateItemContent(item, 0, 0); cut {
		t.Fatal("unset limits must not truncate")
	}
}
