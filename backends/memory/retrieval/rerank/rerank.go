// Package rerank implements a model-backed listwise reranker over hydrated
// context items, so evidence the question actually needs can outrank
// first-stage retrieval noise.
package rerank

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	corememory "github.com/GizClaw/flowcraft/core/memory"
	coremessage "github.com/GizClaw/flowcraft/core/message"
)

// Runtime is the narrow inference surface the reranker needs.
type Runtime interface {
	Generate(context.Context, model.ModelRef, inference.GenerateRequest) (inference.GenerateResponse, error)
}

// Model reranks hydrated items with a generation model.
type Model struct {
	runtime      Runtime
	ref          model.ModelRef
	maxItems     int
	maxItemRunes int
}

// New builds a model-backed item reranker.
func New(runtime Runtime, ref model.ModelRef) (*Model, error) {
	if runtime == nil {
		return nil, errors.New("rerank: runtime is required")
	}
	if strings.TrimSpace(ref.ID.Provider) == "" || strings.TrimSpace(ref.ID.Name) == "" {
		return nil, errors.New("rerank: model provider and name are required")
	}
	return &Model{runtime: runtime, ref: ref, maxItems: 40, maxItemRunes: 400}, nil
}

// RerankItems asks the model for the most relevant items first. Items beyond
// maxItems keep their relative order after the reranked head, and every
// position — head, leftover head, and tail alike — gets a monotonically
// decreasing score over the whole slice. Scoring the head against its own
// length while the tail kept its fused score let tail items outrank the
// reranked head in the packer's score sort.
func (model *Model) RerankItems(ctx context.Context, query string, items []corememory.ContextItem) ([]corememory.ContextItem, error) {
	if model == nil || model.runtime == nil {
		return nil, errors.New("rerank: model is incomplete")
	}
	if ctx == nil {
		return nil, errors.New("rerank: context is required")
	}
	query = strings.TrimSpace(query)
	if query == "" || len(items) == 0 {
		return items, nil
	}
	head := items
	tail := []corememory.ContextItem(nil)
	if len(head) > model.maxItems {
		head = items[:model.maxItems]
		tail = items[model.maxItems:]
	}
	response, err := model.runtime.Generate(ctx, model.ref, generateRequest(query, head, model.maxItemRunes))
	if err != nil {
		return nil, fmt.Errorf("rerank: generate: %w", err)
	}
	order, err := parseOrder(response.Message.Content.Text(), len(head))
	if err != nil {
		return nil, err
	}
	result := make([]corememory.ContextItem, 0, len(items))
	seen := make(map[int]struct{}, len(order))
	for _, index := range order {
		if _, duplicate := seen[index]; duplicate {
			continue
		}
		seen[index] = struct{}{}
		value := head[index]
		// The packer sorts by score, so the reranked position must be
		// reflected in the score; otherwise reordering has no effect.
		value.Score = rerankScore(len(result), len(items))
		result = append(result, value)
	}
	for index := range head {
		if _, ok := seen[index]; ok {
			continue
		}
		value := head[index]
		value.Score = rerankScore(len(result), len(items))
		result = append(result, value)
	}
	for _, value := range tail {
		value.Score = rerankScore(len(result), len(items))
		result = append(result, value)
	}
	return result, nil
}

// rerankScore maps a 0-based position onto (0,1]: the first item scores 1 and
// later items decay monotonically, so the packer's score sort follows the
// reranked order while keeping ties impossible.
func rerankScore(position, total int) float64 {
	if total <= 1 {
		return 1
	}
	return 1 - float64(position)/float64(total)
}

func generateRequest(query string, items []corememory.ContextItem, maxItemRunes int) inference.GenerateRequest {
	intent := inference.Intent{Text: &inference.TextIntent{
		Response: &inference.ResponseFormat{Kind: inference.ResponseJSONObject},
	}}
	return inference.GenerateRequest{
		Context: []coremessage.Message{{
			Role:    coremessage.RoleSystem,
			Content: coremessage.NewTextContent(rerankSystem),
		}},
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: coremessage.NewTextContent(rerankUser(query, items, maxItemRunes)),
				Intent:  intent,
			},
		},
	}
}

func rerankUser(query string, items []corememory.ContextItem, maxItemRunes int) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Question: %s\n\nCandidates:\n", query)
	for index, item := range items {
		fmt.Fprintf(&builder, "C%d: %s\n", index, truncateRunes(strings.TrimSpace(item.Content.Text()), maxItemRunes))
	}
	builder.WriteString("\nReturn the JSON object.")
	return builder.String()
}

func parseOrder(raw string, count int) ([]int, error) {
	var decoded struct {
		Order []int `json:"order"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &decoded); err != nil {
		return nil, fmt.Errorf("rerank: parse response: %w", err)
	}
	if len(decoded.Order) == 0 {
		return nil, errors.New("rerank: model returned an empty order")
	}
	order := make([]int, 0, len(decoded.Order))
	for _, index := range decoded.Order {
		if index < 0 || index >= count {
			return nil, fmt.Errorf("rerank: model returned out-of-range index %d", index)
		}
		order = append(order, index)
	}
	return order, nil
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if max <= 0 || len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "…"
}

// AlgorithmVersion names the rerank policy. Bump it with the prompt: it goes
// into the run fingerprint, and the label has to name what actually runs.
//
// v1 (this one) keeps the order the model returned, mapped straight onto the
// packer's score. A v2 experiment (coverage-aware ordering: strongest first,
// then still-missing pieces, repeats last) was measured on the 282 multi-hop
// questions and lost accuracy (strict 0.546 -> 0.535, lenient 0.848 -> 0.833)
// without changing evidence coverage at all, so it was not kept. Together with
// the diversity-packing experiment (coverage +15.6pp, accuracy -1.1/-2.1pp) it
// says the answering model does best with the most similar items in similarity
// order; the multi-hop gap is not reachable by re-arranging this candidate
// pool.
const AlgorithmVersion = "rerank-v1"

const rerankSystem = `You rank memory candidates by how much they help answer the question.

Rules:
- Judge only the relevance of each candidate to the question; never answer it.
- Rank candidates that contain concrete evidence (names, dates, places, numbers, causes) above vague or merely related ones.
- When the question needs several facts combined, rank every candidate that carries one of those facts highly.
- Return JSON only: {"order": [2, 0, 1]} listing candidate indices most relevant first. Include every candidate exactly once.`
