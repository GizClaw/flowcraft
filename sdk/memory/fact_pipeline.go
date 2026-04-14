package memory

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// ConflictType classifies a contradiction between new and existing memory.
type ConflictType string

const (
	ConflictNone              ConflictType = ""
	ConflictFactualCorrection ConflictType = "factual_correction"
	ConflictPreferenceChange  ConflictType = "preference_change"
	ConflictContextDependent  ConflictType = "context_dependent"
)

// NewFactPipeline builds the default fact extraction pipeline.
func NewFactPipeline(resolver llm.LLMResolver, store LongTermStore, ltConfig LongTermConfig, ecfg ExtractorConfig) *Pipeline {
	return NewPipeline(
		&extractStage{resolver: resolver, ecfg: ecfg},
		&searchStage{store: store, ltConfig: ltConfig},
		&deduplicateStage{ecfg: ecfg},
		&persistStage{store: store, ltConfig: ltConfig},
	)
}

type extractStage struct {
	resolver llm.LLMResolver
	ecfg     ExtractorConfig
}

func (s *extractStage) Name() string { return "extract" }

func (s *extractStage) Run(ctx context.Context, state *PipelineState) error {
	l, err := s.resolver.Resolve(ctx, "")
	if err != nil {
		return fmt.Errorf("resolve LLM: %w", err)
	}
	state.LLM = l

	candidates, err := extractCandidates(ctx, l, state.Input, s.ecfg)
	if err != nil {
		return fmt.Errorf("extract candidates: %w", err)
	}
	if s.ecfg.PostProcess != nil {
		candidates = s.ecfg.PostProcess(candidates, state.Input.Source)
	}
	state.Candidates = candidates
	return nil
}

type searchStage struct {
	store    LongTermStore
	ltConfig LongTermConfig
}

func (s *searchStage) Name() string { return "search" }

func globalCategoriesForExtract(in ExtractInput, lt LongTermConfig) []MemoryCategory {
	if len(in.GlobalCategories) > 0 {
		return in.GlobalCategories
	}
	return lt.GlobalCategories
}

func (s *searchStage) Run(ctx context.Context, state *PipelineState) error {
	enabled := enabledCategorySet(s.ltConfig)
	if len(state.Input.LongTermCategories) > 0 {
		enabled = enabledCategorySetFromStrings(state.Input.LongTermCategories)
	}
	gCats := globalCategoriesForExtract(state.Input, s.ltConfig)
	var toSave []CandidateMemory
	var toDedup []dedupItem

	for _, cand := range state.Candidates {
		if len(enabled) > 0 && !enabled[cand.Category] {
			continue
		}

		useScope := s.ltConfig.ScopeEnabled
		if state.Input.ScopeInExtract != nil {
			useScope = *state.Input.ScopeInExtract
		}
		var scopePtr *MemoryScope
		if useScope {
			sc := NormalizeScopeForCategory(cand.Category, state.Input.Scope, gCats)
			sc.RuntimeID = state.Input.RuntimeID
			if state.Input.StripSessionIDFromSearch {
				sc.SessionID = ""
			}
			scopePtr = &sc
		}

		existing, err := s.store.Search(ctx, state.Input.RuntimeID, cand.Content, SearchOptions{
			Category: cand.Category,
			TopK:     5,
			Scope:    scopePtr,
		})
		if err != nil {
			telemetry.Warn(ctx, "extractor search stage: search failed",
				otellog.String("error", err.Error()),
				otellog.String("category", string(cand.Category)))
			existing = nil
		}
		if len(existing) == 0 {
			toSave = append(toSave, cand)
		} else {
			toDedup = append(toDedup, dedupItem{cand: cand, existing: existing})
		}
	}
	state.DirectSave = toSave
	state.ToDedup = toDedup
	return nil
}

func enabledCategorySet(cfg LongTermConfig) map[MemoryCategory]bool {
	if len(cfg.Categories) == 0 {
		return nil
	}
	return enabledCategorySetFromStrings(cfg.Categories)
}

func enabledCategorySetFromStrings(cats []string) map[MemoryCategory]bool {
	m := make(map[MemoryCategory]bool, len(cats))
	for _, c := range cats {
		m[MemoryCategory(c)] = true
	}
	return m
}

type deduplicateStage struct {
	ecfg ExtractorConfig
}

func (s *deduplicateStage) Name() string { return "deduplicate" }

func (s *deduplicateStage) Run(ctx context.Context, state *PipelineState) error {
	if len(state.ToDedup) == 0 {
		return nil
	}
	actions, err := deduplicateBatch(ctx, state.LLM, s.ecfg, state.ToDedup)
	if err != nil {
		telemetry.Warn(ctx, "extractor: batch dedup failed, falling back to create all",
			otellog.String("error", err.Error()))
		state.DedupFallbackCreateAll = true
		recordDedupFallback(ctx)
		return nil
	}
	state.Actions = actions
	return nil
}

type persistStage struct {
	store    LongTermStore
	ltConfig LongTermConfig
}

func (s *persistStage) Name() string { return "persist" }

func (s *persistStage) Run(ctx context.Context, state *PipelineState) error {
	runtimeID := state.Input.RuntimeID
	source := state.Input.Source
	globalCats := globalCategoriesForExtract(state.Input, s.ltConfig)

	for _, cand := range state.DirectSave {
		entry := memoryEntryFromCandidate(cand, source, state.Input, globalCats)
		if err := s.store.Save(ctx, runtimeID, entry); err != nil {
			telemetry.Warn(ctx, "extractor: save entry failed", otellog.String("error", err.Error()))
			continue
		}
		state.Saved = append(state.Saved, entry)
	}

	if len(state.ToDedup) == 0 {
		return nil
	}

	if state.DedupFallbackCreateAll {
		for _, item := range state.ToDedup {
			entry := memoryEntryFromCandidate(item.cand, source, state.Input, globalCats)
			if err := s.store.Save(ctx, runtimeID, entry); err != nil {
				telemetry.Warn(ctx, "extractor: fallback save failed", otellog.String("error", err.Error()))
				continue
			}
			state.Saved = append(state.Saved, entry)
		}
		return nil
	}

	for i, rawAction := range state.Actions {
		if i >= len(state.ToDedup) {
			break
		}
		cand := state.ToDedup[i].cand
		existing := state.ToDedup[i].existing
		if rawAction.ConflictType != "" {
			recordConflictDecision(ctx, rawAction.ConflictType)
		}
		action := interpretConflict(rawAction, cand)

		entry := memoryEntryFromCandidate(cand, source, state.Input, globalCats)

		var opErr error
		switch action.Action {
		case ActionSkip:
			continue
		case ActionCreate:
			opErr = s.store.Save(ctx, runtimeID, entry)
			if opErr == nil {
				state.Saved = append(state.Saved, entry)
			}
		case ActionMerge:
			if action.TargetID != "" && action.MergedContent != "" {
				matched := false
				for _, ex := range existing {
					if ex.ID == action.TargetID {
						kw := deduplicateKeywords(ex.Keywords, cand.Keywords)
						if action.ConflictType == ConflictPreferenceChange {
							kw = appendPreferenceHistoryKeywords(kw, ex.Content)
						}
						// Current-turn scope (timeline session / user); ex.Scope would keep a stale session_id after cross-session merge.
						merged := &MemoryEntry{
							ID:       ex.ID,
							Category: ex.Category,
							Content:  action.MergedContent,
							Keywords: kw,
							Source:   ex.Source,
							Scope:    entry.Scope,
						}
						opErr = s.store.Update(ctx, runtimeID, merged)
						if opErr == nil {
							state.Saved = append(state.Saved, merged)
						}
						matched = true
						break
					}
				}
				if !matched {
					telemetry.Warn(ctx, "extractor: merge target not found, falling back to create",
						otellog.String("target_id", action.TargetID))
					opErr = s.store.Save(ctx, runtimeID, entry)
					if opErr == nil {
						state.Saved = append(state.Saved, entry)
					}
				}
			} else {
				opErr = s.store.Save(ctx, runtimeID, entry)
				if opErr == nil {
					state.Saved = append(state.Saved, entry)
				}
			}
		case ActionDelete:
			if action.TargetID != "" {
				if err := s.store.Delete(ctx, runtimeID, action.TargetID); err != nil {
					telemetry.Warn(ctx, "extractor: delete failed", otellog.String("error", err.Error()))
				}
			}
			opErr = s.store.Save(ctx, runtimeID, entry)
			if opErr == nil {
				state.Saved = append(state.Saved, entry)
			}
		default:
			opErr = s.store.Save(ctx, runtimeID, entry)
			if opErr == nil {
				state.Saved = append(state.Saved, entry)
			}
		}
		if opErr != nil {
			telemetry.Warn(ctx, "extractor: dedup action failed",
				otellog.String("action", string(action.Action)),
				otellog.String("error", opErr.Error()))
		}
	}
	return nil
}

func memoryEntryFromCandidate(cand CandidateMemory, source MemorySource, in ExtractInput, globalCats []MemoryCategory) *MemoryEntry {
	scope := NormalizeScopeForCategory(cand.Category, in.Scope, globalCats)
	scope.RuntimeID = in.RuntimeID
	if !scope.IsGlobal() {
		sid := in.TimelineSessionID
		if sid == "" && in.Source.ConversationID != "" && in.Source.ConversationID != scope.UserID {
			sid = in.Source.ConversationID
		}
		if sid != "" {
			scope.SessionID = sid
		}
	}
	return &MemoryEntry{
		Category: cand.Category,
		Content:  cand.Content,
		Keywords: cand.Keywords,
		Source:   source,
		Scope:    scope,
	}
}

func interpretConflict(action deduplicationResult, cand CandidateMemory) deduplicationResult {
	a := action
	switch a.ConflictType {
	case ConflictContextDependent:
		if a.Action == ActionDelete {
			a.Action = ActionCreate
			a.TargetID = ""
		}
	case ConflictPreferenceChange:
		if a.Action == ActionDelete {
			a.Action = ActionMerge
			if a.MergedContent == "" {
				a.MergedContent = cand.Content
			}
		}
	}
	return a
}

func appendPreferenceHistoryKeywords(kw []string, oldContent string) []string {
	const maxOld = 80
	r := []rune(oldContent)
	if len(r) > maxOld {
		r = r[:maxOld]
	}
	old := string(r)
	if old == "" {
		return kw
	}
	prefix := "prev:"
	for _, k := range kw {
		if k == prefix+old {
			return kw
		}
	}
	return append(kw, prefix+old)
}
