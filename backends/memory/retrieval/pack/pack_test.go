package pack

import (
	"context"
	"fmt"
	"testing"

	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

type counterFunc func(context.Context, coremessage.Content) (int, error)

func (function counterFunc) Count(ctx context.Context, content coremessage.Content) (int, error) {
	return function(ctx, content)
}

func TestDeterministicBudgetDedupAndTokenCount(t *testing.T) {
	packer := New(counterFunc(func(_ context.Context, content coremessage.Content) (int, error) {
		switch content.Text() {
		case "large":
			return 2, nil
		default:
			return 1, nil
		}
	}))
	items := []corememory.ContextItem{
		item("b", "large", 0.9),
		item("a", "small", 0.9),
		item("a", "duplicate", 0.1),
		item("c", "small", 0.8),
	}
	result, err := packer.Pack(context.Background(), items, corememory.Budget{MaxItems: 3, MaxTokens: 2})
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
	first.Address = corememory.ContextAddress{
		Kind: corememory.ContextFact, ConversationID: "conversation-a", ItemID: first.ID,
	}
	second := item("fact", "second", 1)
	second.Address = corememory.ContextAddress{
		Kind: corememory.ContextFact, ConversationID: "conversation-b", ItemID: second.ID,
	}
	result, err := New(nil).Pack(context.Background(), []corememory.ContextItem{first, second},
		corememory.Budget{MaxItems: 2, MaxTokens: 100})
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
	result, err := New(nil).Pack(context.Background(), []corememory.ContextItem{
		item("recent", "你好世界", 1),
		item("semantic", "later", 1),
	}, corememory.Budget{MaxItems: 2, MaxTokens: 100, MaxChars: 4})
	if err != nil || len(result.Items) != 1 || result.Items[0].ID != "recent" || !result.Truncated {
		t.Fatalf("character budget result = %#v, %v", result, err)
	}
}

func TestDefaultClassBudgetsAndBorrowing(t *testing.T) {
	packer := New(counterFunc(func(context.Context, coremessage.Content) (int, error) { return 1, nil }))
	var items []corememory.ContextItem
	for index := 0; index < 10; index++ {
		recent := item(fmt.Sprintf("recent-%02d", index), "r", 1)
		recent.SourceClass = corememory.ContextSourceRecent
		long := item(fmt.Sprintf("long-%02d", index), "l", 1)
		long.SourceClass = corememory.ContextSourceLongTerm
		summary := item(fmt.Sprintf("summary-%02d", index), "s", 1)
		summary.SourceClass = corememory.ContextSourceSummary
		items = append(items, recent, long, summary)
	}
	result, err := packer.Pack(context.Background(), items, corememory.Budget{MaxItems: 10, MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[corememory.ContextSourceClass]int{}
	for _, got := range result.Items {
		counts[got.SourceClass]++
	}
	if counts[corememory.ContextSourceRecent] != 6 ||
		counts[corememory.ContextSourceLongTerm] != 3 ||
		counts[corememory.ContextSourceSummary] != 1 {
		t.Fatalf("class counts=%v", counts)
	}

	var onlyLong []corememory.ContextItem
	for index := 0; index < 10; index++ {
		got := item(fmt.Sprintf("only-%02d", index), "x", 1)
		got.SourceClass = corememory.ContextSourceLongTerm
		onlyLong = append(onlyLong, got)
	}
	borrowed, err := packer.Pack(context.Background(), onlyLong, corememory.Budget{MaxItems: 10, MaxTokens: 10})
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

// TestClassItemBudgetsReserveLongTermSlots pins the packing fix: the recent
// lane may fill its class share and lend unused capacity, but it cannot
// consume every item slot before long-term candidates get their reserved
// share.
func TestClassItemBudgetsReserveLongTermSlots(t *testing.T) {
	packer := New(counterFunc(func(context.Context, coremessage.Content) (int, error) { return 1, nil }))
	var items []corememory.ContextItem
	for index := 0; index < 20; index++ {
		recent := item(fmt.Sprintf("recent-%02d", index), "r", 1)
		recent.SourceClass = corememory.ContextSourceRecent
		items = append(items, recent)
	}
	for index := 0; index < 10; index++ {
		long := item(fmt.Sprintf("long-%02d", index), "l", 1)
		long.SourceClass = corememory.ContextSourceLongTerm
		items = append(items, long)
	}
	result, err := packer.Pack(context.Background(), items, corememory.Budget{MaxItems: 20, MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[corememory.ContextSourceClass]int{}
	for _, got := range result.Items {
		counts[got.SourceClass]++
	}
	if len(result.Items) != 20 {
		t.Fatalf("items = %d, want 20", len(result.Items))
	}
	if counts[corememory.ContextSourceLongTerm] < 6 {
		t.Fatalf("long-term items = %d, want at least the 30%% class share (6)", counts[corememory.ContextSourceLongTerm])
	}
	if counts[corememory.ContextSourceRecent] > 14 {
		t.Fatalf("recent items = %d, want the recent class to lend at most the unused slots", counts[corememory.ContextSourceRecent])
	}
}

// TestRecentLendsSlotsWhenLongTermAbsent keeps the complementary behavior:
// when no other class has candidates, the recent lane still fills the budget.
func TestRecentLendsSlotsWhenLongTermAbsent(t *testing.T) {
	packer := New(counterFunc(func(context.Context, coremessage.Content) (int, error) { return 1, nil }))
	var items []corememory.ContextItem
	for index := 0; index < 20; index++ {
		recent := item(fmt.Sprintf("recent-%02d", index), "r", 1)
		recent.SourceClass = corememory.ContextSourceRecent
		items = append(items, recent)
	}
	result, err := packer.Pack(context.Background(), items, corememory.Budget{MaxItems: 20, MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 20 {
		t.Fatalf("items = %d, want the recent lane to fill the unused budget", len(result.Items))
	}
}

func TestSystemRecentPriorityAndOversizedItemNeverExceedsTotal(t *testing.T) {
	packer := New(counterFunc(func(_ context.Context, content coremessage.Content) (int, error) {
		if content.Text() == "oversized" {
			return 11, nil
		}
		return 1, nil
	}))
	system := item("system", "system", 0)
	system.SourceClass, system.MessageRole = corememory.ContextSourceRecent, coremessage.RoleSystem
	summary := item("summary", "summary", 1)
	summary.SourceClass = corememory.ContextSourceSummary
	oversized := item("oversized", "oversized", 1)
	oversized.SourceClass = corememory.ContextSourceRecent
	result, err := packer.Pack(context.Background(), []corememory.ContextItem{summary, oversized, system},
		corememory.Budget{MaxItems: 2, MaxTokens: 10})
	if err != nil || len(result.Items) != 2 || result.Items[0].ID != "system" ||
		result.TokenCount > 10 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func item(id, content string, score float64) corememory.ContextItem {
	return corememory.ContextItem{
		ID: id, Kind: corememory.ContextFact, Content: text(content), Score: score,
		Sources:     []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "source-" + id}},
		SourceClass: corememory.ContextSourceLongTerm,
	}
}

func text(value string) coremessage.Content {
	return coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: value}}}
}
