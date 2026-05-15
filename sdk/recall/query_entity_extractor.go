// Package recall — query-time LLM entity extractor.
//
// At write time the additive extractor produces an "entities" field per
// fact: open-vocabulary noun phrases like "Greenwich Community Center"
// or "LGBTQ support group". These are persisted by EntityStore.Link as
// inverted-index keys (normalized → memEntryID list).
//
// At query time the default pipeline extractor (rule-based,
// pipeline.ruleEntities) returns only capitalized single tokens and
// quoted runs. A query like "When did Caroline go to the LGBTQ
// support group?" produces ["caroline", "lgbtq"] — never the noun
// phrase the write side stored, so the EntityStore lookup join never
// hits the discriminative entry. Our entity-store ablation
// (run 25908012719 / 25909308192) showed this asymmetry collapses the
// entity-link contribution to ~baseline regardless of pollution gate
// settings, because the only entities that DO join are speaker
// names — and those get either flood-filtered (gate=0 lane) or
// silently dropped (gate=N, lane skipped).
//
// LLMQueryEntityExtractor closes the asymmetry by using an LLM
// (same model as the write-side extractor) to produce normalised
// entity phrases from the query. It deliberately mirrors the
// write-side prompt rules — atomic proper nouns + discriminative
// noun phrases, no dates / generic nouns / pronouns — so the
// resulting entities lemma-join cleanly with the stored keys.
package recall

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

const defaultQueryEntityPrompt = `You are extracting retrieval entities from a search query.

Return a STRICT JSON object {"entities": ["..."]} listing the discriminative entities in the query — the rare proper nouns and specific noun phrases a retrieval system should use to find the matching memory.

INCLUDE:
- Atomic proper nouns: "Caroline", "Maya", "San Francisco", "Toyota Prius"
- Multi-word specific noun phrases: "LGBTQ support group", "Greenwich Community Center", "graduate art history program"
- Concrete artefacts the query references: book titles, products, brands, identifiers

EXCLUDE:
- Dates, months, days, years ("8 May 2023", "morning", "Tuesday")
- Generic activities ("meeting", "coffee", "school", "trip")
- Pronouns and possessives ("her", "his", "my")
- Common verbs and adjectives ("attend", "favourite")
- Question words ("what", "when", "how long")

Render every entity in lowercase. Strip leading/trailing punctuation. Preserve internal spaces inside multi-word phrases.

If the query has NO discriminative entities, return {"entities": []}.

Examples:

Query: When did Caroline go to the LGBTQ support group?
Output: {"entities": ["caroline", "lgbtq support group"]}

Query: How long has Caroline been transitioning?
Output: {"entities": ["caroline"]}

Query: What did Melanie read last summer?
Output: {"entities": ["melanie"]}

Query: Did Caroline mention any specific therapists at Greenwich Community Center?
Output: {"entities": ["caroline", "greenwich community center"]}

Query: %s

Output:`

// queryEntityResponse mirrors the strict-JSON envelope the prompt
// asks the LLM to emit; tolerates a single top-level "entities" key.
type queryEntityResponse struct {
	Entities []string `json:"entities"`
}

// llmQueryEntityExtractor returns a function suitable for
// [pipeline.WithEntityExtractor]: it invokes the supplied LLM with the
// query-time entity prompt and parses the strict-JSON envelope.
//
// The extractor is best-effort: on LLM error, empty output, or parse
// failure it returns an empty slice + nil error so the pipeline
// gracefully falls back to "no query entities" rather than aborting
// the recall. Only the "rule extractor would have caught more" cost
// is paid; the retrieval path itself stays available.
func llmQueryEntityExtractor(client llm.LLM) func(context.Context, string) ([]string, error) {
	if client == nil {
		return nil
	}
	return func(ctx context.Context, text string) ([]string, error) {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil, nil
		}
		// Render the prompt with the query interpolated where the
		// template expects it. We avoid fmt.Sprintf %s on the
		// raw template so a query containing percent-escapes
		// can't corrupt the request.
		prompt := strings.Replace(defaultQueryEntityPrompt, "%s", text, 1)
		resp, _, err := client.Generate(ctx, []llm.Message{
			{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: prompt}}},
		}, llm.WithJSONMode(true))
		if err != nil {
			return nil, nil
		}
		raw := strings.TrimSpace(resp.Content())
		if raw == "" {
			return nil, nil
		}
		payload, _, jerr := llm.ExtractJSON(raw)
		if jerr != nil {
			return nil, nil
		}
		var env queryEntityResponse
		if uerr := json.Unmarshal(payload, &env); uerr != nil {
			return nil, nil
		}
		out := make([]string, 0, len(env.Entities))
		seen := make(map[string]struct{}, len(env.Entities))
		for _, e := range env.Entities {
			s := strings.ToLower(strings.TrimSpace(e))
			if s == "" {
				continue
			}
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
		return out, nil
	}
}
