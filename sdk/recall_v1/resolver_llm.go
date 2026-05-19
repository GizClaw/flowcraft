package recall

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// LLMUpdateResolver is the reference [UpdateResolver] implementation.
// It asks an LLM to produce per-(new_fact, candidate) decisions in
// strict JSON form, then projects them onto [ResolveAction].
//
// The prompt is batch-aware: every Save's non-slot facts are presented
// together so the model can resolve combined contradictions a
// per-fact loop would miss (e.g. divorce + remarriage in one turn).
//
// A malformed response is propagated as an error so the Save path
// reports it on memory.recall.resolver_actions_total{op="error"} and
// the Save itself still succeeds (errors are logged, not returned).
type LLMUpdateResolver struct {
	LLM            llm.LLM
	PromptTemplate string  // optional override; defaults to DefaultResolverPrompt
	MaxCandidates  int     // truncate candidates passed to the LLM (default 20)
	ConfidenceMin  float64 // skip actions whose JSON "confidence" is below this (0 = accept all)
}

// DefaultResolverPrompt is the prompt template used by
// [LLMUpdateResolver] when no override is configured. It expects two
// %s arguments: the NEW FACTS slate (JSON array, each element has
// id+content+optional source) and the CANDIDATES slate (JSON array,
// id+content). The template intentionally encourages combined
// reasoning across all new facts.
const DefaultResolverPrompt = `You are Flowcraft's memory update resolver. A NEW SAVE just produced one or more NEW FACTS at the same time. For each CANDIDATE memory below, decide whether ANY of the new facts implies an action on it. Consider the new facts JOINTLY — a candidate may be invalidated by one fact and reinforced by another.

For each (candidate, triggering new fact) pair where action != ADD, return one row.

Allowed ops:
- ADD:    candidate is unrelated to all new facts; no action needed (omit from output).
- UPDATE: candidate is superseded by a new fact (e.g. profile change, preference change, relationship change).
- DELETE: candidate is contradicted or negated by a new fact (e.g. "my dog passed away" vs "I have a dog").
- NOOP:   candidate is equivalent to or already contains the relevant new fact.

Rules:
1. Each non-ADD action MUST cite "source_id" — the id of the new fact that triggered it. Hallucinated source_ids are dropped.
2. UPDATE when topic stays but value changes; DELETE only on explicit negation; ambiguous cases fall back to ADD (omit).
3. Episodic events (one-off past actions) almost always resolve to ADD.
4. When two new facts both imply an action on the same candidate, emit BOTH rows; FlowCraft de-duplicates downstream.
5. Output strict JSON in the exact shape below — no prose, no markdown.

OUTPUT JSON:
{
  "actions": [
    {"source_id": "<new fact id>", "target_id": "<candidate id>", "op": "UPDATE", "confidence": 0.0-1.0}
  ]
}

NEW FACTS (JSON):
%s

CANDIDATES (JSON):
%s
`

// resolverActionsSchema is the structured-output schema sent alongside
// the resolver prompt. Mirrors extractedFactsSchema's strict shape so
// providers that enforce strict JSON-schema (Azure OpenAI, etc.)
// accept the call. The schema permits all four ops on the wire even
// though ADD is normally omitted from output — the LLM may still emit
// ADD rows defensively, and the parser keeps them as informational
// no-ops.
var resolverActionsSchema = llm.JSONSchemaParam{
	Name:        "resolver_actions",
	Description: "Memory update resolver decisions for one Save batch",
	Strict:      true,
	Schema: map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"actions"},
		"properties": map[string]any{
			"actions": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"source_id", "target_id", "op", "confidence"},
					"properties": map[string]any{
						"source_id":  map[string]any{"type": "string"},
						"target_id":  map[string]any{"type": "string"},
						"op":         map[string]any{"type": "string", "enum": []string{"ADD", "UPDATE", "DELETE", "NOOP"}},
						"confidence": map[string]any{"type": "number"},
					},
				},
			},
		},
	},
}

type resolverEnvelope struct {
	Actions []resolverAction `json:"actions"`
}

type resolverAction struct {
	SourceID   string  `json:"source_id"`
	TargetID   string  `json:"target_id"`
	Op         string  `json:"op"`
	Confidence float64 `json:"confidence,omitempty"`
}

type resolverNewFactJSON struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Subject   string `json:"subject,omitempty"`
	Predicate string `json:"predicate,omitempty"`
}

type resolverCandidate struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

// Resolve implements [UpdateResolver].
func (r LLMUpdateResolver) Resolve(ctx context.Context, batch ResolveBatch) ([]ResolveAction, error) {
	if r.LLM == nil || len(batch.NewFacts) == 0 || len(batch.Candidates) == 0 {
		return nil, nil
	}
	max := r.MaxCandidates
	if max <= 0 {
		max = 20
	}
	candidates := batch.Candidates
	if len(candidates) > max {
		candidates = candidates[:max]
	}

	newFactsJSON := make([]resolverNewFactJSON, 0, len(batch.NewFacts))
	for _, nf := range batch.NewFacts {
		newFactsJSON = append(newFactsJSON, resolverNewFactJSON{
			ID:        nf.EntryID,
			Content:   nf.Fact.Content,
			Subject:   nf.Fact.Subject,
			Predicate: nf.Fact.Predicate,
		})
	}
	newFactsRaw, err := json.Marshal(newFactsJSON)
	if err != nil {
		return nil, fmt.Errorf("recall: resolver: marshal new facts: %w", err)
	}
	candJSON := make([]resolverCandidate, 0, len(candidates))
	for _, c := range candidates {
		candJSON = append(candJSON, resolverCandidate{
			ID:      c.Entry.ID,
			Content: c.Entry.Content,
		})
	}
	candRaw, err := json.Marshal(candJSON)
	if err != nil {
		return nil, fmt.Errorf("recall: resolver: marshal candidates: %w", err)
	}
	tmpl := r.PromptTemplate
	if tmpl == "" {
		tmpl = DefaultResolverPrompt
	}
	prompt := fmt.Sprintf(tmpl, string(newFactsRaw), string(candRaw))

	resp, _, err := r.LLM.Generate(ctx, []llm.Message{{
		Role:  model.RoleUser,
		Parts: []model.Part{{Type: model.PartText, Text: prompt}},
	}},
		llm.WithJSONSchema(resolverActionsSchema),
		llm.WithJSONMode(true),
	)
	if err != nil {
		return nil, err
	}
	raw := resp.Content()
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	payload, _, err := llm.ExtractJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("recall: resolver: %w", err)
	}
	var env resolverEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, fmt.Errorf("recall: resolver: parse: %w", err)
	}
	out := make([]ResolveAction, 0, len(env.Actions))
	for _, a := range env.Actions {
		if r.ConfidenceMin > 0 && a.Confidence > 0 && a.Confidence < r.ConfidenceMin {
			continue
		}
		op := ResolveOp(strings.ToUpper(strings.TrimSpace(a.Op)))
		switch op {
		case OpAdd, OpUpdate, OpDelete, OpNoop:
		default:
			continue
		}
		out = append(out, ResolveAction{
			Op:       op,
			SourceID: strings.TrimSpace(a.SourceID),
			TargetID: strings.TrimSpace(a.TargetID),
		})
	}
	return out, nil
}
