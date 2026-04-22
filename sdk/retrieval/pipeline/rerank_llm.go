package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// LLMReranker scores candidate hits with a single LLM call and reorders them.
//
// The model receives the query and a numbered list of candidate contents and
// must respond with a JSON object {"ranking":[{"index":int,"score":0..1}]}.
// We multiply the existing Score by score (0..1) so unrated hits sink to the
// bottom; this keeps the contract compatible with downstream Limit / TimeDecay.
//
// Reranker is intentionally framework-agnostic: it depends only on llm.LLM.
type LLMReranker struct {
	LLM llm.LLM
	// Model is reserved for future per-call overrides; the configured llm.LLM's
	// default model is used today.
	Model string
	// MaxBatch caps how many hits are sent in one request (default 30).
	MaxBatch int
	// PromptTemplate overrides the default prompt; %s placeholders are
	// query, then candidate block.
	PromptTemplate string
}

// Rerank implements pipeline.Reranker.
func (r *LLMReranker) Rerank(ctx context.Context, query string, hits []retrieval.Hit) ([]retrieval.Hit, error) {
	if r == nil || r.LLM == nil || len(hits) == 0 {
		return hits, nil
	}
	max := r.MaxBatch
	if max <= 0 {
		max = 30
	}
	candidates := hits
	if len(candidates) > max {
		candidates = candidates[:max]
	}
	var b strings.Builder
	for i, h := range candidates {
		fmt.Fprintf(&b, "[%d] %s\n", i, snippet(h.Doc.Content, 320))
	}
	tmpl := r.PromptTemplate
	if tmpl == "" {
		tmpl = defaultRerankPrompt
	}
	prompt := fmt.Sprintf(tmpl, strings.TrimSpace(query), b.String())

	resp, _, err := r.LLM.Generate(ctx, []llm.Message{
		{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: prompt}}},
	},
		llm.WithJSONSchema(rerankSchema),
		llm.WithJSONMode(true),
	)
	if err != nil {
		return hits, err
	}
	scores, err := parseRerankResponse(resp.Content(), len(candidates))
	if err != nil {
		return hits, err
	}
	out := make([]retrieval.Hit, len(candidates))
	copy(out, candidates)
	for i := range out {
		s := scores[i]
		if out[i].Scores == nil {
			out[i].Scores = map[string]float64{}
		}
		out[i].Scores["rerank_llm"] = s
		// Multiply rather than replace so prior boosts/decays carry through.
		out[i].Score = out[i].Score * (0.1 + 0.9*s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(hits) > len(candidates) {
		// Append untouched tail at the end so the caller still sees them, but
		// they're guaranteed not to outrank reranked candidates.
		out = append(out, hits[len(candidates):]...)
	}
	return out, nil
}

const defaultRerankPrompt = `You are a relevance ranker. Given a user query and a numbered list of candidate memory snippets, score each candidate's relevance to the query.

Query: %s

Candidates:
%s

For each candidate, return a relevance score in [0,1] where:
- 1.0  = directly answers the query or contains the exact fact needed
- 0.5  = related but not directly answering
- 0.0  = unrelated

Respond with a strict JSON object: {"ranking":[{"index":<int>,"score":<float>}]}
Include every candidate index exactly once.`

var rerankSchema = llm.JSONSchemaParam{
	Name:        "rerank_scores",
	Description: "Per-candidate relevance scores",
	Strict:      true,
	Schema: map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"ranking"},
		"properties": map[string]any{
			"ranking": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"index", "score"},
					"properties": map[string]any{
						"index": map[string]any{"type": "integer"},
						"score": map[string]any{"type": "number"},
					},
				},
			},
		},
	},
}

func parseRerankResponse(raw string, n int) ([]float64, error) {
	scores := make([]float64, n)
	for i := range scores {
		scores[i] = 0.5 // neutral default for missing entries
	}
	if strings.TrimSpace(raw) == "" {
		return scores, nil
	}
	payload, _, err := llm.ExtractJSON(raw)
	if err != nil {
		return scores, fmt.Errorf("rerank: %w", err)
	}
	var env struct {
		Ranking []struct {
			Index int     `json:"index"`
			Score float64 `json:"score"`
		} `json:"ranking"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return scores, fmt.Errorf("rerank: parse: %w", err)
	}
	for _, r := range env.Ranking {
		if r.Index < 0 || r.Index >= n {
			continue
		}
		s := r.Score
		if s < 0 {
			s = 0
		}
		if s > 1 {
			s = 1
		}
		scores[r.Index] = s
	}
	return scores, nil
}

func snippet(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
