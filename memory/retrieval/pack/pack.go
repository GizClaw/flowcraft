// Package pack implements deterministic context budget packing.
package pack

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/memory/component"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
)

const (
	defaultMaxItems  = 20
	defaultMaxTokens = 4096
)

type TokenCounter interface {
	Count(context.Context, sdkmessage.Content) (int, error)
}

// Counter is retained as a source-compatible alias.
type Counter = TokenCounter

// RuneCounter estimates ceil(Unicode rune count / 4), with one token for
// non-empty content.
type RuneCounter struct{}

func (RuneCounter) Count(_ context.Context, content sdkmessage.Content) (int, error) {
	text := content.Text()
	if text == "" {
		if len(content.Parts) == 0 {
			return 0, nil
		}
		return 1, nil
	}
	return max(1, (utf8.RuneCountInString(text)+3)/4), nil
}

type ClassBudgets struct {
	Recent  float64
	MidLong float64
	Summary float64
}

type Config struct {
	TokenCounter TokenCounter
	ClassBudgets ClassBudgets
}

type Deterministic struct {
	Counter      TokenCounter
	ClassBudgets ClassBudgets
}

var _ component.Packer = (*Deterministic)(nil)

func New(counter Counter) *Deterministic {
	packer, _ := NewConfigured(Config{TokenCounter: counter})
	return packer
}

func NewConfigured(config Config) (*Deterministic, error) {
	if config.TokenCounter == nil {
		config.TokenCounter = RuneCounter{}
	}
	budgets, err := normalizeBudgets(config.ClassBudgets)
	if err != nil {
		return nil, err
	}
	return &Deterministic{Counter: config.TokenCounter, ClassBudgets: budgets}, nil
}

func (packer *Deterministic) Pack(ctx context.Context, items []sdkmemory.ContextItem, budget sdkmemory.Budget) (sdkmemory.ContextResult, error) {
	if packer == nil || packer.Counter == nil {
		return sdkmemory.ContextResult{}, errors.New("pack: counter is required")
	}
	if ctx == nil {
		return sdkmemory.ContextResult{}, errors.New("pack: context is required")
	}
	if err := budget.Validate(); err != nil {
		return sdkmemory.ContextResult{}, err
	}
	maxItems, maxTokens := budget.MaxItems, budget.MaxTokens
	if maxItems == 0 {
		maxItems = defaultMaxItems
	}
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	owned := make([]sdkmemory.ContextItem, len(items))
	for i, item := range items {
		if err := item.Validate(); err != nil {
			return sdkmemory.ContextResult{}, fmt.Errorf("pack: item %d: %w", i, err)
		}
		owned[i] = cloneItem(item)
	}
	sort.SliceStable(owned, func(i, j int) bool {
		return lessItem(owned[i], owned[j])
	})
	type preparedItem struct {
		item   sdkmemory.ContextItem
		tokens int
		chars  int
		class  int
	}
	prepared := make([]preparedItem, 0, len(owned))
	seen := make(map[string]struct{}, len(owned))
	for _, item := range owned {
		if err := ctx.Err(); err != nil {
			return sdkmemory.ContextResult{}, err
		}
		key := itemAddressKey(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		count, err := packer.Counter.Count(ctx, item.Content)
		if err != nil {
			return sdkmemory.ContextResult{}, fmt.Errorf("pack: count item %q: %w", item.ID, err)
		}
		if count < 0 {
			return sdkmemory.ContextResult{}, fmt.Errorf("pack: counter returned negative count for %q", item.ID)
		}
		item.TokenCount = count
		prepared = append(prepared, preparedItem{
			item: item, tokens: count, chars: utf8.RuneCountInString(item.Content.Text()), class: budgetClass(item),
		})
	}

	caps := tokenCaps(maxTokens, packer.ClassBudgets)
	usedByClass := [3]int{}
	selected := make([]bool, len(prepared))
	result := sdkmemory.ContextResult{Items: make([]sdkmemory.ContextItem, 0, min(maxItems, len(prepared)))}
	usedChars := 0
	fitsGlobal := func(value preparedItem) bool {
		return len(result.Items) < maxItems && result.TokenCount+value.tokens <= maxTokens &&
			(budget.MaxChars == 0 || usedChars+value.chars <= budget.MaxChars)
	}
	add := func(index int) {
		value := prepared[index]
		selected[index] = true
		result.Items = append(result.Items, value.item)
		result.TokenCount += value.tokens
		usedChars += value.chars
		usedByClass[value.class] += value.tokens
	}
	// First pass enforces 0.6/0.3/0.1 reservations.
	for index, value := range prepared {
		if usedByClass[value.class]+value.tokens <= caps[value.class] && fitsGlobal(value) {
			add(index)
		}
	}
	// Empty or undersubscribed classes lend their remaining capacity in fixed
	// recent -> mid/long -> summary priority.
	for class := 0; class < 3; class++ {
		for index, value := range prepared {
			if selected[index] || value.class != class {
				continue
			}
			if fitsGlobal(value) {
				add(index)
			}
		}
	}
	if len(result.Items) < len(prepared) {
		result.Truncated = true
	}
	sort.SliceStable(result.Items, func(i, j int) bool {
		return lessItem(result.Items[i], result.Items[j])
	})
	return result, nil
}

func normalizeBudgets(value ClassBudgets) (ClassBudgets, error) {
	if value == (ClassBudgets{}) {
		return ClassBudgets{Recent: 0.6, MidLong: 0.3, Summary: 0.1}, nil
	}
	values := []float64{value.Recent, value.MidLong, value.Summary}
	total := 0.0
	for _, ratio := range values {
		if ratio < 0 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
			return ClassBudgets{}, errors.New("pack: class budgets must be finite and non-negative")
		}
		total += ratio
	}
	if total <= 0 {
		return ClassBudgets{}, errors.New("pack: class budgets must have a positive sum")
	}
	return ClassBudgets{Recent: value.Recent / total, MidLong: value.MidLong / total, Summary: value.Summary / total}, nil
}

func tokenCaps(total int, budgets ClassBudgets) [3]int {
	recent := int(math.Floor(float64(total) * budgets.Recent))
	midLong := int(math.Floor(float64(total) * budgets.MidLong))
	return [3]int{recent, midLong, total - recent - midLong}
}

func budgetClass(item sdkmemory.ContextItem) int {
	switch item.SourceClass {
	case sdkmemory.ContextSourceRecent:
		return 0
	case sdkmemory.ContextSourceLongTerm:
		return 1
	case sdkmemory.ContextSourceSummary:
		return 2
	default:
		panic(fmt.Sprintf("pack: invalid validated context source class %q", item.SourceClass))
	}
}

func sourcePriority(item sdkmemory.ContextItem) int {
	if item.MessageRole == sdkmessage.RoleSystem {
		return -1
	}
	switch item.SourceClass {
	case sdkmemory.ContextSourceRecent:
		return 0
	case sdkmemory.ContextSourceLongTerm:
		return 1
	case sdkmemory.ContextSourceSummary:
		return 2
	default:
		panic(fmt.Sprintf("pack: invalid validated context source class %q", item.SourceClass))
	}
}

func lessItem(left, right sdkmemory.ContextItem) bool {
	leftPriority, rightPriority := sourcePriority(left), sourcePriority(right)
	if leftPriority != rightPriority {
		return leftPriority < rightPriority
	}
	if left.SourceClass == sdkmemory.ContextSourceRecent && left.Sequence != right.Sequence {
		return left.Sequence < right.Sequence
	}
	if left.Score != right.Score {
		return left.Score > right.Score
	}
	if left.Level != right.Level {
		return left.Level > right.Level
	}
	if left.Kind != right.Kind {
		return left.Kind < right.Kind
	}
	if leftAddress, rightAddress := itemAddressKey(left), itemAddressKey(right); leftAddress != rightAddress {
		return leftAddress < rightAddress
	}
	return left.ID < right.ID
}

func itemAddressKey(item sdkmemory.ContextItem) string {
	if !item.Address.IsZero() {
		return item.Address.Key()
	}
	return string(item.Kind) + "\x00" + item.ID
}

func cloneItem(item sdkmemory.ContextItem) sdkmemory.ContextItem {
	item.Content = item.Content.Clone()
	item.Sources = append([]sdkmemory.SourceRef(nil), item.Sources...)
	if item.Hint != nil {
		hint := item.Hint.Clone()
		item.Hint = &hint
	}
	if item.Metadata != nil {
		metadata := make(sdkmemory.Metadata, len(item.Metadata))
		for key, value := range item.Metadata {
			metadata[key] = value
		}
		item.Metadata = metadata
	}
	return item
}
