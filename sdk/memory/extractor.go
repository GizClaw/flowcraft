package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"

	"go.opentelemetry.io/otel/metric"
)

// Metrics for memory extraction.
var (
	memoryParseFallbacks, _ = telemetry.Meter().Int64Counter("memory_parse_fallbacks_total",
		metric.WithDescription("Total number of JSON parse fallback attempts in memory extraction"))
)

// categoryDescriptions maps built-in categories to their descriptions for the extraction prompt.
var categoryDescriptions = map[MemoryCategory]string{
	CategoryProfile:     "User's basic info (name, role, background)",
	CategoryPreferences: "User's preferences (language, style, tools)",
	CategoryEntities:    "Key entities mentioned (project names, teams, systems)",
	CategoryEvents:      "Important events (decisions, milestones, issues)",
	CategoryCases:       "Successful task completions (task + approach + result)",
	CategoryPatterns:    "Learned patterns (common errors, best practices, feedback)",
}

// RegisterCategoryDescription associates a description with a custom category
// for use in the default extraction prompt.
func RegisterCategoryDescription(cat MemoryCategory, desc string) {
	categoryDescriptions[cat] = desc
}

// DefaultExtractPrompt returns the built-in extraction prompt, dynamically
// including all registered categories.
func DefaultExtractPrompt() string {
	var catLines strings.Builder
	for _, cat := range AllCategories() {
		desc, ok := categoryDescriptions[cat]
		if ok {
			fmt.Fprintf(&catLines, "- %s: %s\n", cat, desc)
		} else {
			fmt.Fprintf(&catLines, "- %s\n", cat)
		}
	}
	return fmt.Sprintf(`Analyze the following conversation and extract information worth remembering long-term.

For each piece of information, classify it into one of these categories:
%s
Return a JSON array of objects with fields: category, content, keywords.
Only extract genuinely useful information. Skip trivial or transient details.
If nothing worth remembering, return an empty array.

Conversation:
%%s

Extracted memories:`, catLines.String())
}

// DefaultDeduplicationPrompt is the built-in batch deduplication prompt.
const DefaultDeduplicationPrompt = `For each new memory below, compare it with its existing memories and decide what to do.

Actions:
- skip: already covered by existing
- create: genuinely new
- merge: complements an existing one (provide target_id + merged_content)
- delete: makes an existing one obsolete (provide target_id)

When action is "delete", also set conflict_type:
- factual_correction: the old fact is objectively wrong now
- preference_change: a preference evolved over time
- context_dependent: both are valid in different contexts (prefer "create" instead of delete)

%s
Respond with JSON array: [{"index": 0, "action": "...", "target_id": "...", "merged_content": "...", "conflict_type": "..."}, ...]`

// DeduplicationAction defines how a candidate memory should be handled.
type DeduplicationAction string

const (
	ActionSkip   DeduplicationAction = "skip"
	ActionCreate DeduplicationAction = "create"
	ActionMerge  DeduplicationAction = "merge"
	ActionDelete DeduplicationAction = "delete"
)

// CandidateMemory represents a memory entry extracted by the LLM before deduplication.
type CandidateMemory struct {
	Category  MemoryCategory `json:"category"`
	Content   string         `json:"content"`
	Keywords  []string       `json:"keywords"`
	Timestamp string         `json:"timestamp,omitempty"`
}

// ExtractInput is the input to MemoryExtractor.Extract.
type ExtractInput struct {
	RuntimeID       string
	Messages        []llm.Message // Messages to extract from (typically incremental).
	ContextMessages []llm.Message // Optional broader context window for the LLM.
	Source          MemorySource
	Scope           MemoryScope

	// StripSessionIDFromSearch, when true, clears SessionID on the scope used in
	// the search/dedup stage so existing memories match across conversation threads.
	// TimelineSessionID (or Source.ConversationID when it differs from Scope.UserID)
	// is still persisted on new entries unless the category is global.
	StripSessionIDFromSearch bool

	// TimelineSessionID is written to MemoryEntry.Scope.SessionID for non-global
	// categories (which chat/thread produced this memory). It does not affect the
	// search stage when StripSessionIDFromSearch is true.
	TimelineSessionID string

	// ScopeInExtract, when non-nil, overrides LongTermConfig.ScopeEnabled for
	// the pipeline search stage (shared extractor vs per-agent settings).
	ScopeInExtract *bool

	// GlobalCategories, when non-empty, overrides LongTermConfig.GlobalCategories
	// for scope normalization in this run (shared extractor vs per-agent settings).
	GlobalCategories []MemoryCategory

	// LongTermCategories, when non-empty, overrides LongTermConfig.Categories for
	// the search/persist pipeline (shared extractor vs per-agent settings).
	LongTermCategories []string
}

// ExtractorConfig customizes the extraction behavior of MemoryExtractor.
// Zero values fall back to built-in defaults.
type ExtractorConfig struct {
	// ExtractPrompt is the prompt template for extraction.
	// Must contain one %s placeholder for the conversation text.
	ExtractPrompt string

	// DeduplicationPrompt is the prompt template for batch deduplication.
	// Must contain one %s placeholder for the comparison text.
	DeduplicationPrompt string

	// FormatMessages converts messages into the conversation text passed to the LLM.
	// When nil, the default "role: content\n" format is used.
	FormatMessages func(messages []llm.Message, source MemorySource) string

	// PostProcess is called after LLM extraction and before deduplication.
	// It can add timestamps, transform categories, filter entries, etc.
	PostProcess func(candidates []CandidateMemory, source MemorySource) []CandidateMemory

	// MaxConversationRunes limits the conversation text length (in runes) sent to the LLM.
	// Defaults to 8000 when zero.
	MaxConversationRunes int
}

type deduplicationResult struct {
	Action        DeduplicationAction `json:"action"`
	TargetID      string              `json:"target_id,omitempty"`
	MergedContent string              `json:"merged_content,omitempty"`
	ConflictType  ConflictType        `json:"conflict_type,omitempty"`
}

type batchDeduplicationResult struct {
	Index         int                 `json:"index"`
	Action        DeduplicationAction `json:"action"`
	TargetID      string              `json:"target_id,omitempty"`
	MergedContent string              `json:"merged_content,omitempty"`
	ConflictType  ConflictType        `json:"conflict_type,omitempty"`
}

type dedupItem struct {
	cand     CandidateMemory
	existing []*MemoryEntry
}

// MemoryExtractor automatically extracts long-term memory entries from
// conversations using LLM analysis and deduplication.
type MemoryExtractor struct {
	resolver          llm.LLMResolver
	store             LongTermStore
	config            LongTermConfig
	ecfg              ExtractorConfig
	pipelineExtractor *PipelineExtractor
}

// NewMemoryExtractor creates an extractor with the given configuration.
// Pass ExtractorConfig{} for default behavior.
func NewMemoryExtractor(resolver llm.LLMResolver, store LongTermStore, config LongTermConfig, ecfg ExtractorConfig) *MemoryExtractor {
	fact := NewFactPipeline(resolver, store, config, ecfg)
	return &MemoryExtractor{
		resolver:          resolver,
		store:             store,
		config:            config,
		ecfg:              ecfg,
		pipelineExtractor: NewPipelineExtractor(fact),
	}
}

// Extract analyzes messages and persists long-term memory entries.
// Uses batch deduplication to minimize LLM calls: at most 1 extraction +
// 1 batch dedup call regardless of candidate count.
func (e *MemoryExtractor) Extract(ctx context.Context, input ExtractInput) error {
	if err := e.pipelineExtractor.Extract(ctx, input); err != nil {
		return fmt.Errorf("extractor: %w", err)
	}
	return nil
}

func extractCandidates(ctx context.Context, l llm.LLM, input ExtractInput, ecfg ExtractorConfig) ([]CandidateMemory, error) {
	var convText string
	if ecfg.FormatMessages != nil {
		convText = ecfg.FormatMessages(input.Messages, input.Source)
	} else {
		convText = defaultFormatMessages(input.Messages)
	}

	if convText == "" {
		return nil, nil
	}

	maxRunes := ecfg.MaxConversationRunes
	if maxRunes <= 0 {
		maxRunes = 8000
	}
	if runes := []rune(convText); len(runes) > maxRunes {
		convText = string(runes[len(runes)-maxRunes:])
	}

	promptTpl := ecfg.ExtractPrompt
	if promptTpl == "" {
		promptTpl = DefaultExtractPrompt()
	}

	prompt := fmt.Sprintf(promptTpl, convText)

	var msgs []llm.Message
	if len(input.ContextMessages) > 0 {
		var ctxText string
		if ecfg.FormatMessages != nil {
			ctxText = ecfg.FormatMessages(input.ContextMessages, input.Source)
		} else {
			ctxText = defaultFormatMessages(input.ContextMessages)
		}
		ctxBudget := maxRunes * 3 / 10
		if r := []rune(ctxText); len(r) > ctxBudget {
			ctxText = string(r[len(r)-ctxBudget:])
		}
		if ctxText != "" {
			msgs = append(msgs, llm.NewTextMessage(llm.RoleSystem,
				"Earlier conversation for reference only. "+
					"Do NOT extract memories from this section.\n\n"+ctxText))
		}
	}
	msgs = append(msgs, llm.NewTextMessage(llm.RoleUser, prompt))

	jsonOpt := jsonOutputOption(l, extractionSchema())
	resp, _, err := l.Generate(ctx, msgs, jsonOpt)
	if err != nil {
		return nil, err
	}

	content := strings.TrimSpace(resp.Content())

	candidates, parseErr := tryParseCandidates(ctx, content)
	if parseErr == nil {
		return candidates, nil
	}

	return nil, fmt.Errorf("extractor: parse candidates: %w (raw_preview: %s)", parseErr, truncate(content, 200))
}

func defaultFormatMessages(messages []llm.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		if msg.Role == llm.RoleSystem {
			continue
		}
		text := msg.Content()
		if text != "" {
			fmt.Fprintf(&b, "%s: %s\n", msg.Role, text)
		}
	}
	return b.String()
}

// tryParseCandidates attempts multiple JSON parsing strategies.
func tryParseCandidates(ctx context.Context, content string) ([]CandidateMemory, error) {
	cleaned := stripCodeFence(content)
	var candidates []CandidateMemory
	if err := json.Unmarshal([]byte(cleaned), &candidates); err == nil {
		return candidates, nil
	}

	// Strategy: single object (some models return one object instead of an array).
	var single CandidateMemory
	if err := json.Unmarshal([]byte(cleaned), &single); err == nil && single.Category != "" {
		memoryParseFallbacks.Add(ctx, 1)
		return []CandidateMemory{single}, nil
	}

	// Strategy: wrapper object containing an array value (e.g. {"facts": [...]}).
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(cleaned), &wrapper); err == nil {
		for _, v := range wrapper {
			var inner []CandidateMemory
			if err := json.Unmarshal(v, &inner); err == nil && len(inner) > 0 {
				memoryParseFallbacks.Add(ctx, 1)
				return inner, nil
			}
		}
	}

	lines := strings.Split(cleaned, "\n")
	var lineCandidates []CandidateMemory
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[") || strings.HasPrefix(line, "]") {
			continue
		}
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			var cand CandidateMemory
			if err := json.Unmarshal([]byte(line), &cand); err == nil && cand.Category != "" {
				lineCandidates = append(lineCandidates, cand)
			}
		}
	}
	if len(lineCandidates) > 0 {
		memoryParseFallbacks.Add(ctx, 1)
		return lineCandidates, nil
	}

	start := strings.Index(cleaned, "[")
	end := strings.LastIndex(cleaned, "]")
	if start != -1 && end != -1 && end > start {
		arrayContent := cleaned[start : end+1]
		if err := json.Unmarshal([]byte(arrayContent), &candidates); err == nil {
			memoryParseFallbacks.Add(ctx, 1)
			return candidates, nil
		}
	}

	memoryParseFallbacks.Add(ctx, 1)
	return nil, fmt.Errorf("all parse strategies failed")
}

func deduplicateBatch(ctx context.Context, l llm.LLM, ecfg ExtractorConfig, items []dedupItem) ([]deduplicationResult, error) {
	var b strings.Builder
	for i, item := range items {
		fmt.Fprintf(&b, "--- New memory #%d ---\n%s\n\nExisting:\n", i, item.cand.Content)
		for _, ex := range item.existing {
			fmt.Fprintf(&b, "- [ID: %s] %s\n", ex.ID, ex.Content)
		}
		b.WriteString("\n")
	}

	promptTpl := ecfg.DeduplicationPrompt
	if promptTpl == "" {
		promptTpl = DefaultDeduplicationPrompt
	}

	prompt := fmt.Sprintf(promptTpl, b.String())
	jsonOpt := jsonOutputOption(l, deduplicationSchema())
	resp, _, err := l.Generate(ctx, []llm.Message{
		llm.NewTextMessage(llm.RoleUser, prompt),
	}, jsonOpt)
	if err != nil {
		return nil, err
	}

	content := stripCodeFence(strings.TrimSpace(resp.Content()))
	batchResults, err := tryParseBatchResults(content)
	if err != nil {
		return nil, fmt.Errorf("batch dedup parse: %w", err)
	}
	return mapBatchResults(batchResults, len(items)), nil
}

func tryParseBatchResults(content string) ([]batchDeduplicationResult, error) {
	var results []batchDeduplicationResult
	if err := json.Unmarshal([]byte(content), &results); err == nil {
		return results, nil
	}

	// Wrapper object: {"decisions": [...]} or any key containing the array.
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &wrapper); err == nil {
		for _, v := range wrapper {
			if err := json.Unmarshal(v, &results); err == nil && len(results) > 0 {
				return results, nil
			}
		}
	}

	if start := strings.Index(content, "["); start != -1 {
		if end := strings.LastIndex(content, "]"); end > start {
			if err := json.Unmarshal([]byte(content[start:end+1]), &results); err == nil {
				return results, nil
			}
		}
	}

	return nil, fmt.Errorf("all batch parse strategies failed")
}

// mapBatchResults maps sparse batch LLM results into a dense slice, defaulting
// missing or invalid entries to ActionCreate.
func mapBatchResults(results []batchDeduplicationResult, count int) []deduplicationResult {
	mapped := make([]deduplicationResult, count)
	for i := range mapped {
		mapped[i] = deduplicationResult{Action: ActionCreate}
	}
	for _, r := range results {
		if r.Index >= 0 && r.Index < count {
			mapped[r.Index] = deduplicationResult{
				Action:        r.Action,
				TargetID:      r.TargetID,
				MergedContent: r.MergedContent,
				ConflictType:  r.ConflictType,
			}
		}
	}
	return mapped
}

func deduplicateKeywords(existing, new []string) []string {
	seen := make(map[string]bool, len(existing))
	result := make([]string, 0, len(existing)+len(new))
	for _, k := range existing {
		if !seen[k] {
			seen[k] = true
			result = append(result, k)
		}
	}
	for _, k := range new {
		if !seen[k] {
			seen[k] = true
			result = append(result, k)
		}
	}
	return result
}

// stripCodeFence removes markdown code-fence wrappers (```json ... ```)
// that some LLM providers add despite JSON mode being requested.
func stripCodeFence(s string) string {
	if strings.HasPrefix(s, "```") {
		if idx := strings.Index(s, "\n"); idx != -1 {
			s = s[idx+1:]
		}
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	return s
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}

// jsonOutputOption returns WithJSONSchema for the given schema.
// Provider-level ModelCaps handles automatic fallback to JSONMode
// when json_schema is not supported.
func jsonOutputOption(_ llm.LLM, schema llm.JSONSchemaParam) llm.GenerateOption {
	return llm.WithJSONSchema(schema)
}

func extractionSchema() llm.JSONSchemaParam {
	return llm.JSONSchemaParam{
		Name:        "memory_extraction",
		Description: "Extracted long-term memories from conversation",
		Strict:      true,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"memories": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"category": map[string]any{
								"type": "string",
								"enum": AllCategoryStrings(),
							},
							"content":  map[string]any{"type": "string"},
							"keywords": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
						"required":             []string{"category", "content", "keywords"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"memories"},
			"additionalProperties": false,
		},
	}
}

func deduplicationSchema() llm.JSONSchemaParam {
	return llm.JSONSchemaParam{
		Name:        "memory_deduplication",
		Description: "Deduplication decisions for candidate memories",
		Strict:      true,
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"decisions": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"index":          map[string]any{"type": "integer"},
							"action":         map[string]any{"type": "string", "enum": []string{"skip", "create", "merge", "delete"}},
							"target_id":      map[string]any{"type": "string"},
							"merged_content": map[string]any{"type": "string"},
							"conflict_type": map[string]any{
								"type": "string",
								"enum": []string{"factual_correction", "preference_change", "context_dependent"},
							},
						},
						"required":             []string{"index", "action", "target_id", "merged_content"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"decisions"},
			"additionalProperties": false,
		},
	}
}
