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

// TestProfileBlockIsAdmittedBeforeTheClassPasses pins the resident block's
// admission order: the retrieved classes can fill every share the budget
// names, and the block is still served -- it is admitted first, so what it
// costs comes out of the retrieved material and never the other way around.
func TestProfileBlockIsAdmittedBeforeTheClassPasses(t *testing.T) {
	packer := New(counterFunc(func(context.Context, coremessage.Content) (int, error) { return 1, nil }))
	var items []corememory.ContextItem
	for index := 0; index < 10; index++ {
		recent := item(fmt.Sprintf("recent-%02d", index), "r", 1)
		recent.SourceClass = corememory.ContextSourceRecent
		long := item(fmt.Sprintf("long-%02d", index), "l", 1)
		items = append(items, recent, long)
	}
	items = append(items, blockItem("conv-1", "b"))
	result, err := packer.Pack(context.Background(), items, corememory.Budget{MaxItems: 10, MaxTokens: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 10 || result.Items[0].SourceClass != corememory.ContextSourceProfile {
		t.Fatalf("items = %+v", result.Items)
	}
	counts := map[corememory.ContextSourceClass]int{}
	for _, got := range result.Items {
		counts[got.SourceClass]++
	}
	// The block's room comes off the retrieved shares, and the reservations
	// still bind on what is left of the budget: 6 recent and 3 mid/long.
	if counts[corememory.ContextSourceProfile] != 1 ||
		counts[corememory.ContextSourceRecent] != 6 ||
		counts[corememory.ContextSourceLongTerm] != 3 {
		t.Fatalf("class counts=%v", counts)
	}
	if !result.Truncated || result.TokenCount != 10 {
		t.Fatalf("result = %+v", result)
	}
}

// TestProfileBlockThatDoesNotFitTheBudgetIsNotServed is the other half of that
// order: the packer does not cut a block in two. A block the request cannot
// hold is not served at all, and the budget goes to the retrieved items.
func TestProfileBlockThatDoesNotFitTheBudgetIsNotServed(t *testing.T) {
	packer := New(counterFunc(func(_ context.Context, content coremessage.Content) (int, error) {
		if content.Text() == "b" {
			return 9, nil
		}
		return 1, nil
	}))
	items := []corememory.ContextItem{blockItem("conv-1", "b")}
	for index := 0; index < 10; index++ {
		recent := item(fmt.Sprintf("recent-%02d", index), "r", 1)
		recent.SourceClass = corememory.ContextSourceRecent
		items = append(items, recent)
	}
	result, err := packer.Pack(context.Background(), items, corememory.Budget{MaxItems: 10, MaxTokens: 5})
	if err != nil {
		t.Fatal(err)
	}
	if result.TokenCount > 5 || !result.Truncated {
		t.Fatalf("result = %+v", result)
	}
	for _, got := range result.Items {
		if got.SourceClass == corememory.ContextSourceProfile {
			t.Fatalf("a block that does not fit was served: %+v", result.Items)
		}
	}
	if len(result.Items) == 0 {
		t.Fatalf("the budget went unspent: %+v", result.Items)
	}
}

func item(id, content string, score float64) corememory.ContextItem {
	return corememory.ContextItem{
		ID: id, Kind: corememory.ContextFact, Content: text(content), Score: score,
		Sources:     []corememory.SourceRef{{Kind: corememory.SourceMessage, ID: "source-" + id}},
		SourceClass: corememory.ContextSourceLongTerm,
	}
}

func blockItem(conversationID, content string) corememory.ContextItem {
	return corememory.ContextItem{
		ID: conversationID, Kind: corememory.ContextProfile, Content: text(content), Score: 1,
		Address: corememory.ContextAddress{
			Kind: corememory.ContextProfile, ConversationID: conversationID, ItemID: conversationID,
		},
		Sources:     []corememory.SourceRef{{Kind: corememory.SourceExternal, ID: "profile/" + conversationID + "/entry"}},
		SourceClass: corememory.ContextSourceProfile,
	}
}

func text(value string) coremessage.Content {
	return coremessage.Content{Parts: []coremessage.Part{coremessage.TextPart{Text: value}}}
}
