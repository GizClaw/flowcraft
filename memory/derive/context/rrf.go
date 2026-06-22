package context

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/memory/derive"
)

const defaultRRFK = 60

// RRFPacker ranks hydrated context items with reciprocal-rank fusion over the
// deterministic item order supplied by the executor.
type RRFPacker struct{}

func (RRFPacker) PackContext(_ context.Context, input derive.ContextPackInput) (derive.ContextPackOutput, error) {
	if len(input.Items) == 0 {
		return derive.ContextPackOutput{}, nil
	}
	ranked := make([]rankedContextItem, 0, len(input.Items))
	for i, item := range input.Items {
		if strings.TrimSpace(item.Text) == "" {
			continue
		}
		score := reciprocalRank(i + 1)
		ranked = append(ranked, rankedContextItem{
			item:  item,
			index: i,
			score: score,
		})
	}
	sortRankedContextItems(ranked)
	out := make([]derive.ContextItem, 0, len(ranked))
	seen := map[string]int{}
	for _, item := range ranked {
		if key := contextItemRankKey(item.item); key != "" {
			if existing, ok := seen[key]; ok {
				if shouldReplaceContextItem(out[existing], item.item) {
					out[existing] = item.item
				}
				continue
			}
			seen[key] = len(out)
		}
		out = append(out, item.item)
	}
	return derive.ContextPackOutput{Items: out}, nil
}

type rankedContextItem struct {
	item  derive.ContextItem
	index int
	score float64
}

func reciprocalRank(rank int) float64 {
	if rank <= 0 {
		return 0
	}
	return 1.0 / float64(defaultRRFK+rank)
}

func sortRankedContextItems(items []rankedContextItem) {
	for i := 1; i < len(items); i++ {
		current := items[i]
		j := i - 1
		for j >= 0 && compareRankedContextItems(current, items[j]) < 0 {
			items[j+1] = items[j]
			j--
		}
		items[j+1] = current
	}
}

func compareRankedContextItems(a, b rankedContextItem) int {
	if a.score > b.score {
		return -1
	}
	if a.score < b.score {
		return 1
	}
	if a.index < b.index {
		return -1
	}
	if a.index > b.index {
		return 1
	}
	return 0
}

func contextItemRankKey(item derive.ContextItem) string {
	if item.Message != nil && item.Message.ConversationID != "" && item.Message.ID != "" {
		return "message:" + item.Message.ConversationID + ":" + item.Message.ID
	}
	if item.Retrieval != nil && strings.TrimSpace(item.Retrieval.Doc.ID) != "" {
		return "retrieval:" + strings.TrimSpace(item.Retrieval.Doc.ID)
	}
	text := strings.TrimSpace(item.Text)
	if text == "" {
		return ""
	}
	return string(item.Kind) + ":text:" + text
}

func shouldReplaceContextItem(existing, candidate derive.ContextItem) bool {
	return existing.Retrieval == nil && candidate.Retrieval != nil
}
