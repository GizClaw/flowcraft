// Package pack implements deterministic context budget packing.
package pack

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/backends/memory/component"
	"github.com/GizClaw/flowcraft/backends/memory/internal/textutil"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

const (
	// DefaultMaxItems is the item cap applied when a budget leaves MaxItems
	// unset.
	DefaultMaxItems = 20
	// DefaultMaxTokens is the token cap applied when a budget leaves
	// MaxTokens unset.
	DefaultMaxTokens = 4096
)

type TokenCounter interface {
	Count(context.Context, coremessage.Content) (int, error)
}

// Counter is retained as a source-compatible alias.
type Counter = TokenCounter

// RuneCounter estimates the token cost of content without a tokenizer:
// ASCII runes cost a quarter token, other non-ASCII runes half a token, and
// CJK/Hangul runes a full token, rounded up with a minimum of one token for
// non-empty content. Pure ASCII keeps the historical ceil(runes/4) result.
type RuneCounter struct{}

func (RuneCounter) Count(_ context.Context, content coremessage.Content) (int, error) {
	return textutil.ContentTokens(content), nil
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

func (packer *Deterministic) Pack(ctx context.Context, items []corememory.ContextItem, budget corememory.Budget) (corememory.ContextResult, error) {
	if packer == nil || packer.Counter == nil {
		return corememory.ContextResult{}, errors.New("pack: counter is required")
	}
	if ctx == nil {
		return corememory.ContextResult{}, errors.New("pack: context is required")
	}
	if err := budget.Validate(); err != nil {
		return corememory.ContextResult{}, err
	}
	maxItems, maxTokens := budget.MaxItems, budget.MaxTokens
	if maxItems == 0 {
		maxItems = DefaultMaxItems
	}
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}
	owned := make([]corememory.ContextItem, len(items))
	for i, item := range items {
		if err := item.Validate(); err != nil {
			return corememory.ContextResult{}, fmt.Errorf("pack: item %d: %w", i, err)
		}
		// The class helpers below panic on an unknown class; check it here so
		// unvalidated input becomes an error instead of a process crash.
		switch item.SourceClass {
		case corememory.ContextSourceRecent, corememory.ContextSourceLongTerm, corememory.ContextSourceSummary:
		default:
			return corememory.ContextResult{}, fmt.Errorf("pack: item %d has unknown source class %q", i, item.SourceClass)
		}
		owned[i] = cloneItem(item)
	}
	sort.SliceStable(owned, func(i, j int) bool {
		return lessItem(owned[i], owned[j])
	})
	prepared := make([]preparedItem, 0, len(owned))
	seen := make(map[string]struct{}, len(owned))
	for _, item := range owned {
		if err := ctx.Err(); err != nil {
			return corememory.ContextResult{}, err
		}
		key := itemAddressKey(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		count, err := packer.Counter.Count(ctx, item.Content)
		if err != nil {
			return corememory.ContextResult{}, fmt.Errorf("pack: count item %q: %w", item.ID, err)
		}
		if count < 0 {
			return corememory.ContextResult{}, fmt.Errorf("pack: counter returned negative count for %q", item.ID)
		}
		item.TokenCount = count
		prepared = append(prepared, preparedItem{
			item: item, tokens: count, chars: utf8.RuneCountInString(item.Content.Text()), class: budgetClass(item),
		})
	}

	caps := tokenCaps(maxTokens, packer.ClassBudgets)
	// Item budgets mirror the class token budgets so one class — in practice
	// the recent lane — cannot consume every item slot before long-term or
	// summary candidates get a chance. Classes with no candidates contribute
	// no budget, and the lending pass below still lets them donate their
	// unused capacity.
	var available [3]int
	for _, value := range prepared {
		available[value.class]++
	}
	itemCaps := itemCaps(maxItems, packer.ClassBudgets, available)
	usedByClass := [3]int{}
	itemsByClass := [3]int{}
	selected := make([]bool, len(prepared))
	result := corememory.ContextResult{Items: make([]corememory.ContextItem, 0, min(maxItems, len(prepared)))}
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
		itemsByClass[value.class]++
	}
	// First pass enforces the 0.6/0.3/0.1 reservations for both tokens and
	// item slots.
	for index, value := range prepared {
		if itemsByClass[value.class] < itemCaps[value.class] &&
			usedByClass[value.class]+value.tokens <= caps[value.class] && fitsGlobal(value) {
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

// itemCaps converts the class token budgets into item-slot budgets. Classes
// without candidates receive no slots, so their share flows to the classes
// that can actually fill it (via the global cap and the lending pass).
func itemCaps(maxItems int, budgets ClassBudgets, available [3]int) [3]int {
	if maxItems <= 0 {
		return [3]int{}
	}
	ratios := [3]float64{budgets.Recent, budgets.MidLong, budgets.Summary}
	var caps [3]int
	total := 0
	for class := range caps {
		if available[class] == 0 {
			continue
		}
		caps[class] = int(math.Floor(float64(maxItems) * ratios[class]))
		if caps[class] < 1 {
			caps[class] = 1
		}
		total += caps[class]
	}
	// Rounding can overshoot on tiny budgets; trim from the largest budgets
	// first while keeping at least one slot for every class that has items.
	for total > maxItems {
		largest := -1
		for class := range caps {
			if caps[class] <= 1 {
				continue
			}
			if largest == -1 || caps[class] > caps[largest] {
				largest = class
			}
		}
		if largest == -1 {
			break
		}
		caps[largest]--
		total--
	}
	return caps
}

// preparedItem is one candidate with its measured size and budget class.
type preparedItem struct {
	item   corememory.ContextItem
	tokens int
	chars  int
	class  int
}

func budgetClass(item corememory.ContextItem) int {
	switch item.SourceClass {
	case corememory.ContextSourceRecent:
		return 0
	case corememory.ContextSourceLongTerm:
		return 1
	case corememory.ContextSourceSummary:
		return 2
	default:
		panic(fmt.Sprintf("pack: invalid validated context source class %q", item.SourceClass))
	}
}

func sourcePriority(item corememory.ContextItem) int {
	if item.MessageRole == coremessage.RoleSystem {
		return -1
	}
	switch item.SourceClass {
	case corememory.ContextSourceRecent:
		return 0
	case corememory.ContextSourceLongTerm:
		return 1
	case corememory.ContextSourceSummary:
		return 2
	default:
		panic(fmt.Sprintf("pack: invalid validated context source class %q", item.SourceClass))
	}
}

func lessItem(left, right corememory.ContextItem) bool {
	leftPriority, rightPriority := sourcePriority(left), sourcePriority(right)
	if leftPriority != rightPriority {
		return leftPriority < rightPriority
	}
	if left.SourceClass == corememory.ContextSourceRecent && left.Sequence != right.Sequence {
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

func itemAddressKey(item corememory.ContextItem) string {
	if !item.Address.IsZero() {
		return item.Address.Key()
	}
	return string(item.Kind) + "\x00" + item.ID
}

func cloneItem(item corememory.ContextItem) corememory.ContextItem {
	item.Content = item.Content.Clone()
	item.Sources = append([]corememory.SourceRef(nil), item.Sources...)
	if item.Metadata != nil {
		metadata := make(corememory.Metadata, len(item.Metadata))
		for key, value := range item.Metadata {
			metadata[key] = value
		}
		item.Metadata = metadata
	}
	return item
}
