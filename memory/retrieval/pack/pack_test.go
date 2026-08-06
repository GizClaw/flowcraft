package pack

import (
	"context"
	"fmt"
	"testing"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

type counterFunc func(context.Context, sdkmessage.Content) (int, error)

func (function counterFunc) Count(ctx context.Context, content sdkmessage.Content) (int, error) {
	return function(ctx, content)
}

func TestDeterministicBudgetDedupAndTokenCount(t *testing.T) {
	packer := New(counterFunc(func(_ context.Context, content sdkmessage.Content) (int, error) {
		switch content.Text() {
		case "large":
			return 2, nil
		default:
			return 1, nil
		}
	}))
	items := []sdkmemory.ContextItem{
		item("b", "large", 0.9),
		item("a", "small", 0.9),
		item("a", "duplicate", 0.1),
		item("c", "small", 0.8),
	}
	result, err := packer.Pack(context.Background(), items, sdkmemory.Budget{MaxItems: 3, MaxTokens: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 || result.Items[0].ID != "a" || result.Items[1].ID != "c" {
		t.Fatalf("items = %+v", result.Items)
	}
	if result.TokenCount != 2 || !result.Truncated || result.Items[0].TokenCount != 1 {
		t.Fatalf("result = %+v", result)
	}
}

func TestDeterministicKeepsEqualLocalIDsFromDifferentConversations(t *testing.T) {
	first := item("fact", "first", 1)
	first.Address = sdkmemory.ContextAddress{
		Kind: sdkmemory.ContextFact, ConversationID: "conversation-a", ItemID: first.ID,
	}
	second := item("fact", "second", 1)
	second.Address = sdkmemory.ContextAddress{
		Kind: sdkmemory.ContextFact, ConversationID: "conversation-b", ItemID: second.ID,
	}
	result, err := New(nil).Pack(context.Background(), []sdkmemory.ContextItem{first, second},
		sdkmemory.Budget{MaxItems: 2, MaxTokens: 100})
	if err != nil || len(result.Items) != 2 {
		t.Fatalf("qualified items = %#v, %v", result.Items, err)
	}
}

func TestRuneCounterRoundsUp(t *testing.T) {
	count, err := (RuneCounter{}).Count(context.Background(), text("12345"))
	if err != nil || count != 2 {
		t.Fatalf("count = %d, %v", count, err)
	}
}

func TestDeterministicAppliesUnifiedCharacterBudget(t *testing.T) {
	result, err := New(nil).Pack(context.Background(), []sdkmemory.ContextItem{
		item("recent", "你好世界", 1),
		item("semantic", "later", 1),
	}, sdkmemory.Budget{MaxItems: 2, MaxTokens: 100, MaxChars: 4})
	if err != nil || len(result.Items) != 1 || result.Items[0].ID != "recent" || !result.Truncated {
		t.Fatalf("character budget result = %#v, %v", result, err)
	}
}

func TestDefaultClassBudgetsAndBorrowing(t *testing.T) {
	packer := New(counterFunc(func(context.Context, sdkmessage.Content) (int, error) { return 1, nil }))
	var items []sdkmemory.ContextItem
	for index := 0; index < 10; index++ {
		recent := item(fmt.Sprintf("recent-%02d", index), "r", 1)
		recent.SourceClass = sdkmemory.ContextSourceRecent
		long := item(fmt.Sprintf("long-%02d", index), "l", 1)
		long.SourceClass = sdkmemory.ContextSourceLongTerm
		summary := item(fmt.Sprintf("summary-%02d", index), "s", 1)
		summary.SourceClass = sdkmemory.ContextSourceSummary
		items = append(items, recent, long, summary)
	}
	result, err := packer.Pack(context.Background(), items, sdkmemory.Budget{MaxItems: 10, MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[sdkmemory.ContextSourceClass]int{}
	for _, got := range result.Items {
		counts[got.SourceClass]++
	}
	if counts[sdkmemory.ContextSourceRecent] != 6 ||
		counts[sdkmemory.ContextSourceLongTerm] != 3 ||
		counts[sdkmemory.ContextSourceSummary] != 1 {
		t.Fatalf("class counts=%v", counts)
	}

	var onlyLong []sdkmemory.ContextItem
	for index := 0; index < 10; index++ {
		got := item(fmt.Sprintf("only-%02d", index), "x", 1)
		got.SourceClass = sdkmemory.ContextSourceLongTerm
		onlyLong = append(onlyLong, got)
	}
	borrowed, err := packer.Pack(context.Background(), onlyLong, sdkmemory.Budget{MaxItems: 10, MaxTokens: 10})
	if err != nil || len(borrowed.Items) != 10 {
		t.Fatalf("borrowed=%d err=%v", len(borrowed.Items), err)
	}
}

func TestClassBudgetNormalizationAndValidation(t *testing.T) {
	packer, err := NewConfigured(Config{ClassBudgets: ClassBudgets{Recent: 6, MidLong: 3, Summary: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if packer.ClassBudgets != (ClassBudgets{Recent: 0.6, MidLong: 0.3, Summary: 0.1}) {
		t.Fatalf("normalized budgets=%+v", packer.ClassBudgets)
	}
	if _, err := NewConfigured(Config{ClassBudgets: ClassBudgets{Recent: -1, MidLong: 1}}); err == nil {
		t.Fatal("negative class budget accepted")
	}
}

func TestSystemRecentPriorityAndOversizedItemNeverExceedsTotal(t *testing.T) {
	packer := New(counterFunc(func(_ context.Context, content sdkmessage.Content) (int, error) {
		if content.Text() == "oversized" {
			return 11, nil
		}
		return 1, nil
	}))
	system := item("system", "system", 0)
	system.SourceClass, system.MessageRole = sdkmemory.ContextSourceRecent, sdkmessage.RoleSystem
	summary := item("summary", "summary", 1)
	summary.SourceClass = sdkmemory.ContextSourceSummary
	oversized := item("oversized", "oversized", 1)
	oversized.SourceClass = sdkmemory.ContextSourceRecent
	result, err := packer.Pack(context.Background(), []sdkmemory.ContextItem{summary, oversized, system},
		sdkmemory.Budget{MaxItems: 2, MaxTokens: 10})
	if err != nil || len(result.Items) != 2 || result.Items[0].ID != "system" ||
		result.TokenCount > 10 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func item(id, content string, score float64) sdkmemory.ContextItem {
	return sdkmemory.ContextItem{
		ID: id, Kind: sdkmemory.ContextFact, Content: text(content), Score: score,
		Sources:     []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "source-" + id}},
		SourceClass: sdkmemory.ContextSourceLongTerm,
	}
}

func text(value string) sdkmessage.Content {
	return sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: value}}}
}
