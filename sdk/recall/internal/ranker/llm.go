package ranker

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
	"github.com/GizClaw/flowcraft/sdk/recall/internal/port"
)

// LLMReranker reorders hits with a single LLM call. The model
// receives the query and a numbered list of candidate fact contents
// and replies with a JSON object {"ranking":[{"index":int,
// "score":0..1}]}; the returned score multiplies the candidate's
// pre-rerank Score so prior boosts/decays still influence the final
// ordering.
//
// Failure modes are graceful: provider errors, malformed JSON, and
// missing-score entries all degrade to the pre-rerank order with the
// underlying error surfaced to the caller via Recall's stage trace.
// A non-fatal trace is the right shape for a precision booster: a
// rerank outage must never cost availability.
//
// The reranker is intentionally framework-agnostic: it depends only
// on llm.LLM, so any provider that the SDK's LLM facade speaks
// (OpenAI, Azure, Qwen, Anthropic, …) plugs in unchanged.
type LLMReranker struct {
	// Client is the llm.LLM that scores candidates. nil disables
	// reranking entirely (Rerank degrades to a no-op so opt-in via
	// recall.WithReranker stays safe).
	Client llm.LLM
	// MaxBatch caps how many hits are sent in one request. The
	// default (50) matches the post-fusion candidate pool for
	// topK=30, so the reranker sees the full pool without
	// paginating. Larger values raise token cost; smaller values
	// risk leaving the right hit unranked at the tail.
	MaxBatch int
	// SnippetMax caps the content-snippet length attached to each
	// candidate in the prompt. Longer snippets cost more tokens
	// but rarely improve ranking — 320 chars is a safe default.
	SnippetMax int
	// Prompt overrides the default rerank framing. The first %s is
	// the query, the second is the candidate block. Leave empty to
	// use the canonical recall-tuned prompt.
	Prompt string
	// ExtraOptions forwards provider-specific llm.GenerateOption
	// values on every rerank call (temperature override, provider
	// extras, …).
	ExtraOptions []llm.GenerateOption
}

var _ port.Reranker = (*LLMReranker)(nil)

// NewLLM wires an llm.LLM with the default batch / snippet caps.
// Use recall.WithReranker(ranker.NewLLM(client)) at construction
// time to opt the Recall pipeline into LLM-backed reordering.
func NewLLM(client llm.LLM) *LLMReranker {
	return &LLMReranker{Client: client}
}

// Rerank implements port.Reranker.
func (r *LLMReranker) Rerank(ctx context.Context, query string, hits []domain.Hit) ([]domain.Hit, error) {
	if r == nil || r.Client == nil || len(hits) == 0 {
		return hits, nil
	}
	batch := r.MaxBatch
	if batch <= 0 {
		batch = 50
	}
	snip := r.SnippetMax
	if snip <= 0 {
		snip = 320
	}
	cands := hits
	if len(cands) > batch {
		cands = cands[:batch]
	}

	var b strings.Builder
	for i, h := range cands {
		fmt.Fprintf(&b, "[%d] %s\n", i, snippetForRerank(h.Fact.Content, snip))
	}
	tmpl := r.Prompt
	if tmpl == "" {
		tmpl = defaultRerankPrompt
	}
	prompt := fmt.Sprintf(tmpl, strings.TrimSpace(query), b.String())

	opts := []llm.GenerateOption{
		llm.WithJSONSchema(rerankSchemaParam),
		llm.WithJSONMode(true),
	}
	opts = append(opts, r.ExtraOptions...)
	resp, _, err := r.Client.Generate(ctx, []llm.Message{
		llm.NewTextMessage(llm.RoleUser, prompt),
	}, opts...)
	if err != nil {
		return hits, fmt.Errorf("recall rerank: llm generate: %w", err)
	}
	scores, err := parseRerankScores(resp.Content(), len(cands))
	if err != nil {
		return hits, fmt.Errorf("recall rerank: parse: %w", err)
	}
	out := make([]domain.Hit, len(cands))
	copy(out, cands)
	for i := range out {
		s := scores[i]
		// Multiply rather than replace so prior boosts/decays carry
		// through. The 0.1 + 0.9*s floor stops a single 0.0 score
		// from collapsing the candidate's score to zero (which
		// would equate "irrelevant" with "missing" — distinct
		// signals).
		out[i].Score = out[i].Score * (0.1 + 0.9*s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(hits) > len(cands) {
		// Tail beyond MaxBatch is appended verbatim. The
		// reranker's reordered prefix is guaranteed to outrank
		// the un-reranked tail because we never inflate scores.
		out = append(out, hits[len(cands):]...)
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

var rerankSchemaParam = llm.JSONSchemaParam{
	Name:        "recall_rerank_scores",
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

func parseRerankScores(raw string, n int) ([]float64, error) {
	scores := make([]float64, n)
	for i := range scores {
		scores[i] = 0.5 // neutral default for missing entries
	}
	if strings.TrimSpace(raw) == "" {
		return scores, nil
	}
	payload, _, err := llm.ExtractJSON(raw)
	if err != nil {
		return scores, err
	}
	var env struct {
		Ranking []struct {
			Index int     `json:"index"`
			Score float64 `json:"score"`
		} `json:"ranking"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return scores, err
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

func snippetForRerank(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
