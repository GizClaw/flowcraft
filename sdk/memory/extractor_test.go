package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

func TestMapBatchResults_Normal(t *testing.T) {
	results := []batchDeduplicationResult{
		{Index: 0, Action: ActionSkip},
		{Index: 1, Action: ActionCreate},
		{Index: 2, Action: ActionMerge, TargetID: "ex1", MergedContent: "merged"},
	}
	mapped := mapBatchResults(results, 3)
	if len(mapped) != 3 {
		t.Fatalf("expected 3, got %d", len(mapped))
	}
	if mapped[0].Action != ActionSkip {
		t.Fatalf("expected skip, got %s", mapped[0].Action)
	}
	if mapped[1].Action != ActionCreate {
		t.Fatalf("expected create, got %s", mapped[1].Action)
	}
	if mapped[2].Action != ActionMerge || mapped[2].TargetID != "ex1" {
		t.Fatalf("expected merge with target ex1, got %+v", mapped[2])
	}
}

func TestMapBatchResults_MissingIndex(t *testing.T) {
	// Only index 0 provided, index 1 should default to create
	results := []batchDeduplicationResult{
		{Index: 0, Action: ActionSkip},
	}
	mapped := mapBatchResults(results, 2)
	if mapped[0].Action != ActionSkip {
		t.Fatalf("expected skip, got %s", mapped[0].Action)
	}
	if mapped[1].Action != ActionCreate {
		t.Fatalf("missing index should default to create, got %s", mapped[1].Action)
	}
}

func TestMapBatchResults_IndexOutOfBounds(t *testing.T) {
	results := []batchDeduplicationResult{
		{Index: 99, Action: ActionSkip},
		{Index: 0, Action: ActionDelete, TargetID: "t1"},
	}
	mapped := mapBatchResults(results, 2)
	if mapped[0].Action != ActionDelete {
		t.Fatalf("expected delete, got %s", mapped[0].Action)
	}
	if mapped[1].Action != ActionCreate {
		t.Fatalf("out-of-bounds index should be ignored, default create, got %s", mapped[1].Action)
	}
}

func TestMapBatchResults_Empty(t *testing.T) {
	mapped := mapBatchResults(nil, 3)
	if len(mapped) != 3 {
		t.Fatalf("expected 3, got %d", len(mapped))
	}
	for i, m := range mapped {
		if m.Action != ActionCreate {
			t.Fatalf("index %d: expected create default, got %s", i, m.Action)
		}
	}
}

func TestDeduplicateKeywords(t *testing.T) {
	tests := []struct {
		name     string
		existing []string
		new      []string
		want     int
	}{
		{"no overlap", []string{"a", "b"}, []string{"c", "d"}, 4},
		{"full overlap", []string{"a", "b"}, []string{"a", "b"}, 2},
		{"partial overlap", []string{"a", "b", "c"}, []string{"b", "c", "d"}, 4},
		{"empty existing", nil, []string{"a", "b"}, 2},
		{"empty new", []string{"a", "b"}, nil, 2},
		{"both empty", nil, nil, 0},
		{"duplicates in existing", []string{"a", "a", "b"}, []string{"c"}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := deduplicateKeywords(tt.existing, tt.new)
			if len(result) != tt.want {
				t.Fatalf("expected %d keywords, got %d: %v", tt.want, len(result), result)
			}
			// Verify no duplicates
			seen := make(map[string]bool)
			for _, k := range result {
				if seen[k] {
					t.Fatalf("duplicate keyword found: %q", k)
				}
				seen[k] = true
			}
		})
	}
}

func TestStripCodeFence(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no fence", `[{"category":"profile"}]`, `[{"category":"profile"}]`},
		{"json fence", "```json\n[{\"category\":\"profile\"}]\n```", `[{"category":"profile"}]`},
		{"plain fence", "```\n[{\"category\":\"profile\"}]\n```", `[{"category":"profile"}]`},
		{"no trailing fence", "```json\n[{\"category\":\"profile\"}]", `[{"category":"profile"}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripCodeFence(tt.input)
			if got != tt.want {
				t.Fatalf("stripCodeFence(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"short", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"truncated", "hello world", 5, "hello..."},
		{"cjk safe", "你好世界测试", 4, "你好世界..."},
		{"empty", "", 5, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncate(tt.input, tt.maxLen)
			if got != tt.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestTryParseCandidates_DirectJSON(t *testing.T) {
	input := `[{"category":"profile","content":"User is a developer","keywords":["dev"]}]`
	candidates, err := tryParseCandidates(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Category != "profile" {
		t.Fatalf("expected profile, got %q", candidates[0].Category)
	}
}

func TestTryParseCandidates_WithCodeFence(t *testing.T) {
	input := "```json\n[{\"category\":\"events\",\"content\":\"meeting\",\"keywords\":[]}]\n```"
	candidates, err := tryParseCandidates(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Category != "events" {
		t.Fatalf("unexpected: %+v", candidates)
	}
}

func TestTryParseCandidates_LineByLine(t *testing.T) {
	input := "Here are the memories:\n{\"category\":\"profile\",\"content\":\"test\",\"keywords\":[]}\n{\"category\":\"events\",\"content\":\"event\",\"keywords\":[]}"
	candidates, err := tryParseCandidates(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("expected 2 line-by-line candidates, got %d", len(candidates))
	}
}

func TestTryParseCandidates_ArrayExtraction(t *testing.T) {
	input := "Sure! Here are the results: [{\"category\":\"profile\",\"content\":\"developer\",\"keywords\":[\"dev\"]}] hope this helps!"
	candidates, err := tryParseCandidates(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 from array extraction, got %d", len(candidates))
	}
}

func TestTryParseCandidates_AllFail(t *testing.T) {
	input := "I cannot extract any memories from this conversation."
	_, err := tryParseCandidates(context.Background(), input)
	if err == nil {
		t.Fatal("expected error when all strategies fail")
	}
}

func TestTryParseCandidates_EmptyArray(t *testing.T) {
	input := "[]"
	candidates, err := tryParseCandidates(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("expected 0 from empty array, got %d", len(candidates))
	}
}

func TestMergeFallbackToCreate(t *testing.T) {
	// When ActionMerge has an unmatched TargetID, Extract should fall back to Save (create).
	existingID := "ex1"
	unmatchedTargetID := "nonexistent"
	saveCalled := false
	var savedEntry *MemoryEntry

	store := &mergeFallbackStore{
		existing: []*MemoryEntry{{ID: existingID, Category: CategoryProfile, Content: "old", Keywords: []string{"old"}}},
		onSave: func(_ context.Context, _ string, e *MemoryEntry) error {
			saveCalled = true
			savedEntry = e
			return nil
		},
	}

	resolver := &mergeFallbackResolver{
		generate: func(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
			userContent := ""
			if len(msgs) > 0 {
				userContent = msgs[0].Content()
			}
			// First call: extract; second call: batch dedup
			if strings.Contains(userContent, "Analyze the following") {
				return llm.Message{
					Role:  llm.RoleAssistant,
					Parts: []llm.Part{{Type: llm.PartText, Text: `[{"category":"profile","content":"User is a developer","keywords":["dev"]}]`}},
				}, llm.TokenUsage{}, nil
			}
			// Batch dedup returns merge with unmatched target
			return llm.Message{
				Role:  llm.RoleAssistant,
				Parts: []llm.Part{{Type: llm.PartText, Text: `[{"index":0,"action":"merge","target_id":"` + unmatchedTargetID + `","merged_content":"merged"}]`}},
			}, llm.TokenUsage{}, nil
		},
	}

	extractor := NewMemoryExtractor(resolver, store, LongTermConfig{}, ExtractorConfig{})
	ctx := context.Background()
	err := extractor.Extract(ctx, ExtractInput{
		RuntimeID: "agent1",
		Messages: []llm.Message{
			llm.NewTextMessage(llm.RoleUser, "I am a developer"),
		},
		Source: MemorySource{RuntimeID: "owner"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !saveCalled {
		t.Fatal("expected Save (create) to be called when merge target not found")
	}
	if savedEntry == nil || savedEntry.Content != "User is a developer" {
		t.Fatalf("expected create with original content, got %+v", savedEntry)
	}
}

type mergeFallbackStore struct {
	existing []*MemoryEntry
	onSave   func(context.Context, string, *MemoryEntry) error
}

func (m *mergeFallbackStore) Save(ctx context.Context, agentID string, entry *MemoryEntry) error {
	if m.onSave != nil {
		return m.onSave(ctx, agentID, entry)
	}
	return nil
}
func (m *mergeFallbackStore) List(context.Context, string, ListOptions) ([]*MemoryEntry, error) {
	return nil, nil
}
func (m *mergeFallbackStore) Search(_ context.Context, _ string, _ string, opts SearchOptions) ([]*MemoryEntry, error) {
	return m.existing, nil
}
func (m *mergeFallbackStore) Update(context.Context, string, *MemoryEntry) error { return nil }
func (m *mergeFallbackStore) Delete(context.Context, string, string) error       { return nil }

type mergeFallbackResolver struct {
	generate func(context.Context, []llm.Message, ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error)
}

func (m *mergeFallbackResolver) Resolve(ctx context.Context, model string) (llm.LLM, error) {
	return &mergeFallbackLLM{generate: m.generate}, nil
}
func (m *mergeFallbackResolver) InvalidateCache(provider string) {}

type mergeFallbackLLM struct {
	generate func(context.Context, []llm.Message, ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error)
}

func (m *mergeFallbackLLM) Generate(ctx context.Context, msgs []llm.Message, opts ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
	return m.generate(ctx, msgs, opts...)
}
func (m *mergeFallbackLLM) GenerateStream(context.Context, []llm.Message, ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}

func TestDefaultFormatMessages(t *testing.T) {
	msgs := []llm.Message{
		llm.NewTextMessage(llm.RoleSystem, "You are a helpful assistant"),
		llm.NewTextMessage(llm.RoleUser, "Hello"),
		llm.NewTextMessage(llm.RoleAssistant, "Hi there!"),
	}
	result := defaultFormatMessages(msgs)
	if strings.Contains(result, "system") {
		t.Fatal("system messages should be skipped")
	}
	if !strings.Contains(result, "user: Hello") {
		t.Fatalf("expected 'user: Hello', got %q", result)
	}
	if !strings.Contains(result, "assistant: Hi there!") {
		t.Fatalf("expected 'assistant: Hi there!', got %q", result)
	}
}

func TestTryParseCandidates_WithTimestamp(t *testing.T) {
	input := `[{"category":"events","content":"User had a meeting","keywords":["meeting"],"timestamp":"2026年1月15日"}]`
	candidates, err := tryParseCandidates(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	if candidates[0].Timestamp != "2026年1月15日" {
		t.Fatalf("expected timestamp '2026年1月15日', got %q", candidates[0].Timestamp)
	}
}

func TestExtractWithCustomFormatMessages(t *testing.T) {
	customFormatCalled := false
	store := &mergeFallbackStore{}
	resolver := &mergeFallbackResolver{
		generate: func(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
			return llm.Message{
				Role:  llm.RoleAssistant,
				Parts: []llm.Part{{Type: llm.PartText, Text: `[]`}},
			}, llm.TokenUsage{}, nil
		},
	}

	ecfg := ExtractorConfig{
		FormatMessages: func(messages []llm.Message, source MemorySource) string {
			customFormatCalled = true
			var b strings.Builder
			for _, msg := range messages {
				if msg.Role == llm.RoleSystem {
					continue
				}
				fmt.Fprintf(&b, "[custom] %s: %s\n", msg.Role, msg.Content())
			}
			return b.String()
		},
	}

	extractor := NewMemoryExtractor(resolver, store, LongTermConfig{}, ecfg)
	err := extractor.Extract(context.Background(), ExtractInput{
		RuntimeID: "agent1",
		Messages:  []llm.Message{llm.NewTextMessage(llm.RoleUser, "hello")},
		Source:    MemorySource{RuntimeID: "owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !customFormatCalled {
		t.Fatal("expected custom FormatMessages to be called")
	}
}

func TestExtractWithPostProcess(t *testing.T) {
	postProcessCalled := false
	saveCalled := false

	store := &mergeFallbackStore{
		onSave: func(_ context.Context, _ string, e *MemoryEntry) error {
			saveCalled = true
			if !strings.Contains(e.Content, "[processed]") {
				t.Fatalf("expected post-processed content, got %q", e.Content)
			}
			return nil
		},
	}

	resolver := &mergeFallbackResolver{
		generate: func(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
			return llm.Message{
				Role:  llm.RoleAssistant,
				Parts: []llm.Part{{Type: llm.PartText, Text: `[{"category":"profile","content":"User is a dev","keywords":["dev"]}]`}},
			}, llm.TokenUsage{}, nil
		},
	}

	ecfg := ExtractorConfig{
		PostProcess: func(candidates []CandidateMemory, source MemorySource) []CandidateMemory {
			postProcessCalled = true
			for i := range candidates {
				candidates[i].Content = "[processed] " + candidates[i].Content
			}
			return candidates
		},
	}

	extractor := NewMemoryExtractor(resolver, store, LongTermConfig{}, ecfg)
	err := extractor.Extract(context.Background(), ExtractInput{
		RuntimeID: "agent1",
		Messages:  []llm.Message{llm.NewTextMessage(llm.RoleUser, "I am a dev")},
		Source:    MemorySource{RuntimeID: "owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !postProcessCalled {
		t.Fatal("expected PostProcess to be called")
	}
	if !saveCalled {
		t.Fatal("expected Save to be called with post-processed content")
	}
}

func TestExtractWithPostProcess_FilterAll(t *testing.T) {
	saveCalled := false

	store := &mergeFallbackStore{
		onSave: func(_ context.Context, _ string, _ *MemoryEntry) error {
			saveCalled = true
			return nil
		},
	}

	resolver := &mergeFallbackResolver{
		generate: func(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
			return llm.Message{
				Role:  llm.RoleAssistant,
				Parts: []llm.Part{{Type: llm.PartText, Text: `[{"category":"profile","content":"User is a dev","keywords":["dev"]}]`}},
			}, llm.TokenUsage{}, nil
		},
	}

	ecfg := ExtractorConfig{
		PostProcess: func(_ []CandidateMemory, _ MemorySource) []CandidateMemory {
			return nil // filter all
		},
	}

	extractor := NewMemoryExtractor(resolver, store, LongTermConfig{}, ecfg)
	err := extractor.Extract(context.Background(), ExtractInput{
		RuntimeID: "agent1",
		Messages:  []llm.Message{llm.NewTextMessage(llm.RoleUser, "I am a dev")},
		Source:    MemorySource{RuntimeID: "owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if saveCalled {
		t.Fatal("expected no Save when PostProcess filters all candidates")
	}
}

func TestExtractWithContextMessages(t *testing.T) {
	var receivedPrompt string

	store := &mergeFallbackStore{}
	resolver := &mergeFallbackResolver{
		generate: func(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
			if len(msgs) > 0 {
				receivedPrompt = msgs[0].Content()
			}
			return llm.Message{
				Role:  llm.RoleAssistant,
				Parts: []llm.Part{{Type: llm.PartText, Text: `[]`}},
			}, llm.TokenUsage{}, nil
		},
	}

	extractor := NewMemoryExtractor(resolver, store, LongTermConfig{}, ExtractorConfig{})
	err := extractor.Extract(context.Background(), ExtractInput{
		RuntimeID: "agent1",
		Messages:  []llm.Message{llm.NewTextMessage(llm.RoleUser, "incremental only")},
		ContextMessages: []llm.Message{
			llm.NewTextMessage(llm.RoleUser, "earlier context"),
			llm.NewTextMessage(llm.RoleAssistant, "earlier reply"),
			llm.NewTextMessage(llm.RoleUser, "incremental only"),
		},
		Source: MemorySource{RuntimeID: "owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(receivedPrompt, "earlier context") {
		t.Fatal("expected ContextMessages to be used instead of Messages")
	}
}

func TestExtractorConfigZeroValue(t *testing.T) {
	store := &mergeFallbackStore{}
	resolver := &mergeFallbackResolver{
		generate: func(_ context.Context, msgs []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
			prompt := msgs[0].Content()
			if !strings.Contains(prompt, "Analyze the following conversation") {
				t.Fatal("expected DefaultExtractPrompt to be used with zero-value config")
			}
			return llm.Message{
				Role:  llm.RoleAssistant,
				Parts: []llm.Part{{Type: llm.PartText, Text: `[]`}},
			}, llm.TokenUsage{}, nil
		},
	}

	extractor := NewMemoryExtractor(resolver, store, LongTermConfig{}, ExtractorConfig{})
	err := extractor.Extract(context.Background(), ExtractInput{
		RuntimeID: "agent1",
		Messages:  []llm.Message{llm.NewTextMessage(llm.RoleUser, "hello")},
		Source:    MemorySource{RuntimeID: "owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExtract_ParseFailureReturnsError(t *testing.T) {
	store := &mergeFallbackStore{}
	resolver := &mergeFallbackResolver{
		generate: func(_ context.Context, _ []llm.Message, _ ...llm.GenerateOption) (llm.Message, llm.TokenUsage, error) {
			return llm.Message{
				Role:  llm.RoleAssistant,
				Parts: []llm.Part{{Type: llm.PartText, Text: "I cannot extract any memories from this."}},
			}, llm.TokenUsage{}, nil
		},
	}

	extractor := NewMemoryExtractor(resolver, store, LongTermConfig{}, ExtractorConfig{})
	err := extractor.Extract(context.Background(), ExtractInput{
		RuntimeID: "agent1",
		Messages:  []llm.Message{llm.NewTextMessage(llm.RoleUser, "hello")},
		Source:    MemorySource{RuntimeID: "owner"},
	})
	if err == nil {
		t.Fatal("expected error when LLM returns unparseable response")
	}
	if !strings.Contains(err.Error(), "parse candidates") {
		t.Fatalf("error should mention parse failure, got: %v", err)
	}
}

func TestExtract_DefaultPromptIncludesRegisteredCategories(t *testing.T) {
	RegisterCategory("test_extract_cat")
	RegisterCategoryDescription("test_extract_cat", "Test category for extraction")

	prompt := DefaultExtractPrompt()
	if !strings.Contains(prompt, "test_extract_cat") {
		t.Fatal("DefaultExtractPrompt should include registered categories")
	}
	if !strings.Contains(prompt, "Test category for extraction") {
		t.Fatal("DefaultExtractPrompt should include category descriptions")
	}
}
