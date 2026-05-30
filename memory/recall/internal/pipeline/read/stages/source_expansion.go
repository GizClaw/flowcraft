package stages

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/words"
)

const (
	sourceExpansionMaxVariants = 4
	sourceExpansionMinBudget   = 8
	sourceExpansionMaxExtra    = 20
)

func querySourceWithPlanVariants(ctx context.Context, src port.Source, plan domain.QueryPlan) domain.SourceResult {
	variants := sourceFanoutPlanVariants(plan, src.Name())
	if len(variants) == 1 {
		return src.Query(ctx, plan)
	}
	if src.Name() != planner.SourceRetrieval {
		results := make([]domain.SourceResult, 0, len(variants))
		for _, variant := range variants {
			results = append(results, src.Query(ctx, variant))
		}
		return mergeVariantSourceResults(src.Name(), plan, results)
	}
	results := make([]domain.SourceResult, len(variants))
	var wg sync.WaitGroup
	wg.Add(len(variants))
	for i, variant := range variants {
		i, variant := i, variant
		go func() {
			defer wg.Done()
			results[i] = src.Query(ctx, variant)
		}()
	}
	wg.Wait()
	return mergeVariantSourceResults(src.Name(), plan, results)
}

func sourceFanoutPlanVariants(plan domain.QueryPlan, sourceName string) []domain.QueryPlan {
	texts := sourceExpansionQueryTexts(plan)
	if len(texts) <= 1 {
		return []domain.QueryPlan{plan}
	}
	out := make([]domain.QueryPlan, 0, len(texts))
	for i, text := range texts {
		variant := plan
		variant.Intent.Text = text
		if i > 0 {
			variant.SourceBudgets = cloneSourceBudgets(plan.SourceBudgets)
			variant.SourceBudgets[sourceName] = sourceExpansionVariantBudget(plan.SourceBudgets[sourceName], plan.TotalCap)
		}
		out = append(out, variant)
	}
	return out
}

func sourceExpansionQueryTexts(plan domain.QueryPlan) []string {
	text := strings.TrimSpace(plan.Intent.Text)
	if text == "" || !sourceExpansionEnabled(plan.TaskIntents) {
		return []string{text}
	}
	var out []string
	seen := map[string]struct{}{}
	add := func(s string) {
		s = strings.Join(strings.Fields(s), " ")
		if s == "" {
			return
		}
		key := strings.ToLower(s)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	add(text)
	add(words.SignificantQueryText(text))
	if hasTask(plan.TaskIntents, domain.QueryTaskBridgeResolution) {
		for _, clause := range words.BridgeClauses(text) {
			add(words.SignificantQueryText(clause))
		}
	}
	if hasTask(plan.TaskIntents, domain.QueryTaskSetCompletion) {
		add(anchorQueryText(text, words.CollectionAnchorWords(text)))
	}
	if hasTask(plan.TaskIntents, domain.QueryTaskTemporalReasoning) {
		add(words.SignificantQueryText(words.StripTemporalQuestionWords(text)))
	}
	if len(out) > sourceExpansionMaxVariants {
		out = out[:sourceExpansionMaxVariants]
	}
	return out
}

func sourceExpansionEnabled(tasks []domain.QueryTaskIntent) bool {
	return hasTask(tasks, domain.QueryTaskSetCompletion) ||
		hasTask(tasks, domain.QueryTaskBridgeResolution) ||
		hasTask(tasks, domain.QueryTaskTemporalReasoning) ||
		hasTask(tasks, domain.QueryTaskDisambiguation)
}

func sourceExpansionVariantBudget(original, totalCap int) int {
	if original <= 0 {
		original = totalCap
	}
	if original <= 0 {
		return sourceExpansionMinBudget
	}
	budget := original / 3
	if budget < sourceExpansionMinBudget {
		budget = sourceExpansionMinBudget
	}
	if budget > original {
		budget = original
	}
	return budget
}

func sourceExpansionMergedCap(plan domain.QueryPlan, sourceName string) int {
	budget := plan.SourceBudgets[sourceName]
	if budget <= 0 {
		budget = plan.TotalCap
	}
	if budget <= 0 {
		return sourceExpansionMinBudget
	}
	extra := budget / 2
	if extra > sourceExpansionMaxExtra {
		extra = sourceExpansionMaxExtra
	}
	return budget + extra
}

func mergeVariantSourceResults(sourceName string, plan domain.QueryPlan, results []domain.SourceResult) domain.SourceResult {
	if len(results) == 0 {
		return domain.SourceResult{Source: sourceName}
	}
	merged := domain.SourceResult{Source: sourceName}
	byFactID := map[string]int{}
	var errs []error
	for resultIdx, res := range results {
		if res.Source != "" {
			merged.Source = res.Source
		}
		merged.Latency += res.Latency
		merged.Truncated = merged.Truncated || res.Truncated
		if res.Err != nil {
			errs = append(errs, res.Err)
		}
		for _, candidate := range res.Candidates {
			if candidate.FactID == "" {
				continue
			}
			candidate.Source = merged.Source
			if resultIdx > 0 {
				candidate.Score = 0
			}
			if existing, ok := byFactID[candidate.FactID]; ok {
				merged.Candidates[existing] = mergeVariantCandidate(merged.Candidates[existing], candidate)
				continue
			}
			byFactID[candidate.FactID] = len(merged.Candidates)
			merged.Candidates = append(merged.Candidates, candidate)
		}
	}
	if cap := sourceExpansionMergedCap(plan, sourceName); cap > 0 && len(merged.Candidates) > cap {
		merged.Candidates = merged.Candidates[:cap]
		merged.Truncated = true
	}
	for i := range merged.Candidates {
		merged.Candidates[i].Rank = i + 1
	}
	merged.Err = errors.Join(errs...)
	return merged
}

func mergeVariantCandidate(existing, incoming domain.Candidate) domain.Candidate {
	out := existing
	out.EvidenceIDs = mergeSourceExpansionEvidenceIDs(out.EvidenceIDs, incoming.EvidenceIDs)
	if incoming.Score > out.Score || (incoming.Score == out.Score && incoming.Rank < out.Rank) {
		out.Score = incoming.Score
		out.Rank = incoming.Rank
		out.Scope = incoming.Scope
		out.Source = incoming.Source
		out.Metadata = cloneCandidateMetadata(incoming.Metadata)
	}
	if out.Metadata == nil {
		out.Metadata = cloneCandidateMetadata(existing.Metadata)
	}
	return out
}

func cloneCandidateMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mergeSourceExpansionEvidenceIDs(existing, incoming []string) []string {
	if len(incoming) == 0 {
		return existing
	}
	out := append([]string(nil), existing...)
	seen := make(map[string]struct{}, len(out)+len(incoming))
	for _, id := range out {
		if id != "" {
			seen[id] = struct{}{}
		}
	}
	for _, id := range incoming {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func cloneSourceBudgets(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func anchorQueryText(text string, anchors []string) string {
	terms := words.SignificantQueryTerms(text)
	if len(anchors) == 0 {
		return strings.Join(terms, " ")
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(anchors)+len(terms))
	for _, term := range append(anchors, terms...) {
		key := strings.ToLower(term)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, term)
	}
	return strings.Join(out, " ")
}
