package stages

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
	"github.com/GizClaw/flowcraft/memory/recall/internal/planner"
	"github.com/GizClaw/flowcraft/memory/recall/internal/port"
	"github.com/GizClaw/flowcraft/memory/recall/internal/words"
	"github.com/GizClaw/flowcraft/memory/text/tokenize"
)

const (
	sourceExpansionMaxVariants = 4
	sourceExpansionMinBudget   = 8
	sourceExpansionMaxExtra    = 20
	sourceExpansionRankPenalty = 32
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
	if !sourceExpansionHasQueryAnchor(plan.Intent) {
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
	if hasTask(plan.TaskIntents, domain.QueryTaskTemporalReasoning) {
		add(words.SignificantQueryText(words.StripTemporalQuestionWords(text)))
	}
	if len(out) > sourceExpansionMaxVariants {
		out = out[:sourceExpansionMaxVariants]
	}
	return out
}

func sourceExpansionHasQueryAnchor(intent domain.QueryIntent) bool {
	if strings.TrimSpace(intent.Subject) != "" ||
		strings.TrimSpace(intent.Predicate) != "" ||
		strings.TrimSpace(intent.Object) != "" ||
		len(intent.Entities) > 0 {
		return true
	}
	features := intent.Features
	return len(features.Proper) > 0 ||
		len(features.Numeric) > 0 ||
		len(features.Quoted) > 0 ||
		features.HasTimeSignal() ||
		sourceExpansionTextHasProperNoun(intent.Text)
}

func sourceExpansionTextHasProperNoun(text string) bool {
	for _, token := range tokenize.SplitProperNouns(text) {
		if words.IsIntentEntityStopword(strings.ToLower(token)) {
			continue
		}
		runes := []rune(token)
		if len(runes) < 2 || !unicode.IsUpper(runes[0]) {
			continue
		}
		if hasLower := slicesContainsFunc(runes[1:], unicode.IsLower); hasLower {
			return true
		}
	}
	return false
}

func slicesContainsFunc[T any](values []T, fn func(T) bool) bool {
	for _, value := range values {
		if fn(value) {
			return true
		}
	}
	return false
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
	byCandidateKey := map[string]int{}
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
		for candidateIdx, candidate := range res.Candidates {
			if candidate.ID == "" {
				continue
			}
			candidate.Source = merged.Source
			if candidate.Rank <= 0 {
				candidate.Rank = candidateIdx + 1
			}
			if resultIdx > 0 {
				candidate.Rank += resultIdx * sourceExpansionRankPenalty
			}
			key := sourceExpansionCandidateKey(candidate)
			if existing, ok := byCandidateKey[key]; ok {
				merged.Candidates[existing] = mergeVariantCandidate(merged.Candidates[existing], candidate)
				continue
			}
			byCandidateKey[key] = len(merged.Candidates)
			merged.Candidates = append(merged.Candidates, candidate)
		}
	}
	sort.SliceStable(merged.Candidates, func(i, j int) bool {
		if merged.Candidates[i].Rank != merged.Candidates[j].Rank {
			return merged.Candidates[i].Rank < merged.Candidates[j].Rank
		}
		return sourceExpansionCandidateKey(merged.Candidates[i]) < sourceExpansionCandidateKey(merged.Candidates[j])
	})
	if cap := sourceExpansionMergedCap(plan, sourceName); cap > 0 && len(merged.Candidates) > cap {
		merged.Candidates = merged.Candidates[:cap]
		merged.Truncated = true
	}
	merged.Err = errors.Join(errs...)
	return merged
}

func mergeVariantCandidate(existing, incoming domain.Candidate) domain.Candidate {
	out := existing
	out.EvidenceIDs = mergeSourceExpansionEvidenceIDs(out.EvidenceIDs, incoming.EvidenceIDs)
	if incoming.Rank < out.Rank || (incoming.Rank == out.Rank && incoming.Score > out.Score) {
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

func sourceExpansionCandidateKey(c domain.Candidate) string {
	return string(c.Kind) + "|" + c.Scope.CanonicalKey() + "|" + c.ID
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
