package retrieval

import (
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/backends/memory/internal/textutil"
	"github.com/GizClaw/flowcraft/backends/memory/retrieval/pack"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// Hard ceilings on model-visible context. Configuration validation uses the
// same numbers, and request-level limits are clamped to them so a single
// caller or config typo cannot ask the provider to build an unbounded context.
const (
	MaxContextItems  = 500
	MaxContextTokens = 65536
	MaxContextChars  = 1 << 20 // 1 Mi runes

	MaxRecentItems  = 500
	MaxRecentTokens = 65536
)

// ClampBudget bounds a caller-supplied budget. Zero keeps its "unset, use the
// packer default" meaning.
func ClampBudget(budget corememory.Budget) corememory.Budget {
	budget.MaxItems = clampLimit(budget.MaxItems, MaxContextItems)
	budget.MaxTokens = clampLimit(budget.MaxTokens, MaxContextTokens)
	budget.MaxChars = clampLimit(budget.MaxChars, MaxContextChars)
	return budget
}

func clampLimit(value, max int) int {
	if value <= 0 || value <= max {
		return value
	}
	return max
}

// EffectivePackLimits resolves the packer defaults for a budget, so callers
// can bound individual items to the same total the packer will enforce.
func EffectivePackLimits(budget corememory.Budget) (maxItems, maxTokens int) {
	maxItems, maxTokens = budget.MaxItems, budget.MaxTokens
	if maxItems == 0 {
		maxItems = pack.DefaultMaxItems
	}
	if maxTokens == 0 {
		maxTokens = pack.DefaultMaxTokens
	}
	return maxItems, maxTokens
}

// TruncateItemContent bounds one context item to maxTokens estimated tokens
// and maxChars runes. Text parts are truncated rune-safely; non-text parts are
// preserved. It reports whether any content was cut and refreshes
// item.TokenCount so packing accounts for the bounded size.
func TruncateItemContent(item corememory.ContextItem, maxTokens, maxChars int) (corememory.ContextItem, bool) {
	if maxTokens <= 0 && maxChars <= 0 {
		return item, false
	}
	text := item.Content.Text()
	if (maxTokens <= 0 || textutil.EstimatedTokens(text) <= maxTokens) &&
		(maxChars <= 0 || utf8.RuneCountInString(text) <= maxChars) {
		return item, false
	}
	content := item.Content.Clone()
	remainingTokens := maxTokens
	remainingChars := maxChars
	changed := false
	for index, part := range content.Parts {
		textPart, ok := part.(coremessage.TextPart)
		if !ok {
			continue
		}
		bounded := textPart.Text
		if remainingTokens > 0 {
			cut, truncated := textutil.TruncateToTokens(bounded, remainingTokens)
			if truncated {
				bounded, changed = cut, true
			}
		} else if maxTokens > 0 && bounded != "" {
			bounded, changed = "", true
		}
		if remainingChars > 0 {
			if runes := []rune(bounded); len(runes) > remainingChars {
				bounded, changed = string(runes[:remainingChars]), true
			}
		} else if maxChars > 0 && bounded != "" {
			bounded, changed = "", true
		}
		if maxTokens > 0 {
			remainingTokens -= textutil.EstimatedTokens(bounded)
		}
		if maxChars > 0 {
			remainingChars -= utf8.RuneCountInString(bounded)
		}
		if bounded != textPart.Text {
			textPart.Text = bounded
			content.Parts[index] = textPart
		}
	}
	if !changed {
		return item, false
	}
	item.Content = content
	item.TokenCount = textutil.EstimatedTokens(content.Text())
	return item, true
}
