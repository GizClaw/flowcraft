package recall

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// inMemConvStore is a minimal short-term store used by aware/assembler tests.
// We declare it locally so this package does not depend on sdk/memory's
// InMemoryStore + BufferMemory, which would create an import cycle.
type inMemConvStore struct {
	mu   sync.Mutex
	data map[string][]model.Message
}

func newConvStore() *inMemConvStore { return &inMemConvStore{data: map[string][]model.Message{}} }

func (s *inMemConvStore) SaveMessages(_ context.Context, id string, msgs []model.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]model.Message, len(msgs))
	copy(cp, msgs)
	s.data[id] = cp
	return nil
}

func (s *inMemConvStore) GetMessages(_ context.Context, id string) ([]model.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]model.Message, len(s.data[id]))
	copy(cp, s.data[id])
	return cp, nil
}

// bufferMemory is the test-side equivalent of sdk/memory.BufferMemory: it
// just round-trips messages through the store. Long-term assembly is added
// by wrapping it with MemoryAwareMemory.
type bufferMemory struct {
	store *inMemConvStore
	max   int
}

func newBufferMemory(s *inMemConvStore, maxN int) *bufferMemory {
	return &bufferMemory{store: s, max: maxN}
}

func (b *bufferMemory) Load(ctx context.Context, id string) ([]model.Message, error) {
	msgs, err := b.store.GetMessages(ctx, id)
	if err != nil {
		return nil, err
	}
	if b.max > 0 && len(msgs) > b.max {
		msgs = msgs[len(msgs)-b.max:]
	}
	return msgs, nil
}

func (b *bufferMemory) Save(ctx context.Context, id string, msgs []model.Message) error {
	return b.store.SaveMessages(ctx, id, msgs)
}

func (b *bufferMemory) Clear(ctx context.Context, id string) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	delete(b.store.data, id)
	return nil
}

// Test convenience aliases so the body of the original test file can stay verbatim.
var (
	NewInMemoryStore = newConvStore
	NewBufferMemory  = newBufferMemory
)

func TestMemoryAwareMemory_NoLTStore(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	inner := NewBufferMemory(store, 50)
	aware := NewMemoryAwareMemoryCompat(inner, nil, "user-1", LongTermConfig{Enabled: false})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1, got %d", len(msgs))
	}
}

func TestMemoryAwareMemory_Injection(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleSystem, "original system"),
		model.NewTextMessage(model.RoleUser, "tell me about golang"),
	})

	ltStore := &inMemoryLTStore{
		entries: map[string][]*MemoryEntry{
			"profile": {
				{ID: "e1", Category: CategoryProfile, Content: "User is a Go developer", Keywords: []string{"golang", "go"}},
			},
		},
	}

	inner := NewBufferMemory(store, 50)
	aware := NewMemoryAwareMemoryCompat(inner, ltStore, "user-1", LongTermConfig{Enabled: true})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}

	if msgs[0].Role != model.RoleSystem {
		t.Fatal("first message should be system")
	}
	content := msgs[0].Content()
	if len(content) == 0 {
		t.Fatal("system message should have content")
	}
}

func TestMemoryAwareMemory_SaveDelegates(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	inner := NewBufferMemory(store, 50)
	aware := NewMemoryAwareMemoryCompat(inner, nil, "user-1", LongTermConfig{})

	_ = aware.Save(ctx, "c1", []model.Message{model.NewTextMessage(model.RoleUser, "hi")})
	msgs, _ := store.GetMessages(ctx, "c1")
	if len(msgs) != 1 {
		t.Fatal("save should delegate to inner")
	}
}

func TestExtractQueryFromMessages(t *testing.T) {
	msgs := []model.Message{
		model.NewTextMessage(model.RoleSystem, "sys"),
		model.NewTextMessage(model.RoleUser, "first"),
		model.NewTextMessage(model.RoleAssistant, "reply"),
		model.NewTextMessage(model.RoleUser, "second"),
	}
	q := extractQueryFromMessages(msgs)
	// Enhanced: combines last N user messages (first, second) for better recall on short follow-ups
	if q != "first second" {
		t.Fatalf("expected 'first second', got %q", q)
	}
}

func TestFormatLongTermMemory(t *testing.T) {
	entries := []*MemoryEntry{
		{Category: CategoryProfile, Content: "User is a developer"},
		{Category: CategoryPreferences, Content: "Prefers Chinese"},
	}
	result := formatLongTermMemory(entries)
	if result == "" {
		t.Fatal("expected non-empty")
	}
}

func TestMemoryAwareMemory_UserIDInjection(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "tell me about golang"),
	})

	ltStore := &inMemoryLTStore{
		entries: map[string][]*MemoryEntry{
			"profile": {
				{ID: "e1", Category: CategoryProfile, Content: "Go developer", Keywords: []string{"go"}},
			},
		},
	}

	inner := NewBufferMemory(store, 50)

	// Empty runtimeID should skip LT retrieval
	aware := NewMemoryAwareMemoryCompat(inner, ltStore, "", LongTermConfig{Enabled: true})
	msgs, _ := aware.Load(ctx, "c1")
	for _, m := range msgs {
		if strings.Contains(m.Content(), "[Long-term memory]") {
			t.Fatal("should not inject when runtimeID is empty")
		}
	}

	// After SetRuntimeID, should inject
	aware.SetRuntimeID("owner")
	msgs, _ = aware.Load(ctx, "c1")
	found := false
	for _, m := range msgs {
		if strings.Contains(m.Content(), "[Long-term memory]") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("should inject long-term memory after SetRuntimeID")
	}
}

func TestExtractQueryFromMessages_ShortReply(t *testing.T) {
	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "tell me about Go concurrency"),
		model.NewTextMessage(model.RoleAssistant, "Go uses goroutines..."),
		model.NewTextMessage(model.RoleUser, "好的"),
	}
	q := extractQueryFromMessages(msgs)
	if !strings.Contains(q, "concurrency") {
		t.Fatalf("short reply should include prior context, got %q", q)
	}
	if !strings.Contains(q, "好的") {
		t.Fatalf("should include current message, got %q", q)
	}
}

func TestExtractQueryFromMessages_MultiTurn(t *testing.T) {
	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "first question"),
		model.NewTextMessage(model.RoleAssistant, "first reply"),
		model.NewTextMessage(model.RoleUser, "second question"),
		model.NewTextMessage(model.RoleAssistant, "second reply"),
		model.NewTextMessage(model.RoleUser, "third question"),
		model.NewTextMessage(model.RoleAssistant, "third reply"),
		model.NewTextMessage(model.RoleUser, "fourth question"),
	}
	q := extractQueryFromMessages(msgs)
	// Should contain last 3 user messages
	if !strings.Contains(q, "second question") {
		t.Fatalf("should include second question, got %q", q)
	}
	if !strings.Contains(q, "fourth question") {
		t.Fatalf("should include fourth question, got %q", q)
	}
	// Should NOT include first question (only last 3)
	if strings.Contains(q, "first question") {
		t.Fatalf("should not include first question (only last 3), got %q", q)
	}
}

func TestExtractQueryFromMessages_Truncation(t *testing.T) {
	// Create a message with lots of CJK characters
	longMsg := strings.Repeat("你好世界", 200) // 800 runes
	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, longMsg),
	}
	q := extractQueryFromMessages(msgs)
	runes := []rune(q)
	if len(runes) > 500 {
		t.Fatalf("query should be truncated to 500 runes, got %d", len(runes))
	}
	// Verify it's valid UTF-8 (if it wasn't, strings.Contains would behave oddly)
	if !strings.Contains(q, "你好") {
		t.Fatal("truncated query should still contain valid CJK text")
	}
}

func TestExtractQueryFromMessages_SingleMessage(t *testing.T) {
	msgs := []model.Message{
		model.NewTextMessage(model.RoleUser, "only one message"),
	}
	q := extractQueryFromMessages(msgs)
	if q != "only one message" {
		t.Fatalf("expected exact message, got %q", q)
	}
}

// inMemoryLTStore is a simple in-memory LongTermStore for testing.
type inMemoryLTStore struct {
	entries map[string][]*MemoryEntry // category -> entries
}

func (s *inMemoryLTStore) Save(_ context.Context, _ string, entry *MemoryEntry) error {
	s.entries[string(entry.Category)] = append(s.entries[string(entry.Category)], entry)
	return nil
}
func (s *inMemoryLTStore) List(_ context.Context, _ string, opts ListOptions) ([]*MemoryEntry, error) {
	if opts.Category != "" {
		return s.entries[string(opts.Category)], nil
	}
	var all []*MemoryEntry
	for _, entries := range s.entries {
		all = append(all, entries...)
	}
	return all, nil
}
func (s *inMemoryLTStore) Search(_ context.Context, _ string, query string, opts SearchOptions) ([]*MemoryEntry, error) {
	all, _ := s.List(context.TODO(), "", ListOptions{Category: opts.Category})
	return all, nil
}
func (s *inMemoryLTStore) Update(_ context.Context, _ string, entry *MemoryEntry) error { return nil }
func (s *inMemoryLTStore) Delete(_ context.Context, _, _ string) error                  { return nil }

func TestMemoryAwareMemory_PinnedCategories(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "write a sorting algorithm"),
	})

	ltStore := &inMemoryLTStore{
		entries: map[string][]*MemoryEntry{
			"profile":     {{ID: "p1", Category: CategoryProfile, Content: "User is a Go developer"}},
			"preferences": {{ID: "pref1", Category: CategoryPreferences, Content: "Prefers concise code"}},
			"events":      {{ID: "ev1", Category: CategoryEvents, Content: "Deployed service v2 last week"}},
		},
	}

	inner := NewBufferMemory(store, 50)
	aware := NewMemoryAwareMemoryCompat(inner, ltStore, "user-1", LongTermConfig{Enabled: true})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}

	system := msgs[0].Content()
	if !strings.Contains(system, "Go developer") {
		t.Fatal("pinned profile should always be injected")
	}
	if !strings.Contains(system, "concise code") {
		t.Fatal("pinned preferences should always be injected")
	}
	if !strings.Contains(system, "Deployed service") {
		t.Fatal("recalled events should be injected when query matches")
	}
}

func TestMemoryAwareMemory_PinnedWithoutQuery(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	// Only system message, no user message → empty query
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleSystem, "system prompt"),
	})

	ltStore := &inMemoryLTStore{
		entries: map[string][]*MemoryEntry{
			"profile": {{ID: "p1", Category: CategoryProfile, Content: "User is a Python developer"}},
			"events":  {{ID: "ev1", Category: CategoryEvents, Content: "Bug fixed yesterday"}},
		},
	}

	inner := NewBufferMemory(store, 50)
	aware := NewMemoryAwareMemoryCompat(inner, ltStore, "user-1", LongTermConfig{Enabled: true})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}

	system := msgs[0].Content()
	// Profile should still be injected even with no user query
	if !strings.Contains(system, "Python developer") {
		t.Fatal("pinned profile should be injected even without a user query")
	}
	// Events should NOT be injected since there's no query for recall
	if strings.Contains(system, "Bug fixed") {
		t.Fatal("recall categories should not be injected without a query")
	}
}

func TestMemoryAwareMemory_ConcurrentSetRuntimeID(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "hi"),
	})

	inner := NewBufferMemory(store, 50)
	aware := NewMemoryAwareMemoryCompat(inner, nil, "", LongTermConfig{Enabled: false})

	// Concurrent SetRuntimeID + Load should not race
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			aware.SetRuntimeID("runtime-" + string(rune('A'+i%26)))
		}
	}()

	for i := 0; i < 100; i++ {
		_, _ = aware.Load(ctx, "c1")
	}
	<-done
}

func TestMemoryAwareMemory_CustomPinnedCategories(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "tell me something"),
	})

	ltStore := &inMemoryLTStore{
		entries: map[string][]*MemoryEntry{
			"profile": {{ID: "p1", Category: CategoryProfile, Content: "Go developer"}},
			"people":  {{ID: "ppl1", Category: "people", Content: "Sister lives in Beijing"}},
			"events":  {{ID: "ev1", Category: CategoryEvents, Content: "Deployed v2 last week"}},
		},
	}

	inner := NewBufferMemory(store, 50)
	aware := NewMemoryAwareMemoryCompat(inner, ltStore, "user-1", LongTermConfig{
		Enabled:          true,
		PinnedCategories: []MemoryCategory{CategoryProfile, "people"},
		RecallCategories: []MemoryCategory{CategoryEvents},
	})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}

	system := msgs[0].Content()
	if !strings.Contains(system, "Go developer") {
		t.Fatal("profile should be pinned")
	}
	if !strings.Contains(system, "Sister lives in Beijing") {
		t.Fatal("custom pinned category 'people' should be injected")
	}
	if !strings.Contains(system, "Deployed v2") {
		t.Fatal("recall category 'events' should be injected when query is present")
	}
}

func TestMemoryAwareMemory_CustomPinned_NoQuery(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleSystem, "system prompt"),
	})

	ltStore := &inMemoryLTStore{
		entries: map[string][]*MemoryEntry{
			"people": {{ID: "ppl1", Category: "people", Content: "Has a sister"}},
			"events": {{ID: "ev1", Category: CategoryEvents, Content: "Bug fixed yesterday"}},
		},
	}

	inner := NewBufferMemory(store, 50)
	aware := NewMemoryAwareMemoryCompat(inner, ltStore, "user-1", LongTermConfig{
		Enabled:          true,
		PinnedCategories: []MemoryCategory{"people"},
		RecallCategories: []MemoryCategory{CategoryEvents},
	})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}

	system := msgs[0].Content()
	if !strings.Contains(system, "Has a sister") {
		t.Fatal("custom pinned should inject even without user query")
	}
	if strings.Contains(system, "Bug fixed") {
		t.Fatal("recall should not inject without a user query")
	}
}

func TestMemoryAwareMemory_DefaultCategoriesBackward(t *testing.T) {
	store := NewInMemoryStore()
	ctx := context.Background()
	_ = store.SaveMessages(ctx, "c1", []model.Message{
		model.NewTextMessage(model.RoleUser, "golang concurrency"),
	})

	ltStore := &inMemoryLTStore{
		entries: map[string][]*MemoryEntry{
			"profile": {{ID: "p1", Category: CategoryProfile, Content: "Dev user"}},
			"events":  {{ID: "ev1", Category: CategoryEvents, Content: "Go 1.25 released"}},
		},
	}

	inner := NewBufferMemory(store, 50)
	// Empty PinnedCategories/RecallCategories → should use defaults
	aware := NewMemoryAwareMemoryCompat(inner, ltStore, "user-1", LongTermConfig{
		Enabled: true,
	})

	msgs, err := aware.Load(ctx, "c1")
	if err != nil {
		t.Fatal(err)
	}

	system := msgs[0].Content()
	if !strings.Contains(system, "Dev user") {
		t.Fatal("default pinned (profile) should work when config is empty")
	}
	if !strings.Contains(system, "Go 1.25") {
		t.Fatal("default recall (events) should work when config is empty")
	}
}

func TestQueryChangeRatio(t *testing.T) {
	tests := []struct {
		name     string
		old, new string
		wantLow  float64
		wantHigh float64
	}{
		{"identical", "hello world", "hello world", 0, 0.001},
		{"completely different", "abc", "xyz", 0.99, 1.01},
		{"empty both", "", "", 0, 0.001},
		{"prefix match", "hello world", "hello", 0.5, 0.6},
		{"one char diff", "abcd", "abce", 0.2, 0.3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ratio := queryChangeRatio(tt.old, tt.new)
			if ratio < tt.wantLow || ratio > tt.wantHigh {
				t.Errorf("queryChangeRatio(%q, %q) = %v, want [%v, %v]", tt.old, tt.new, ratio, tt.wantLow, tt.wantHigh)
			}
		})
	}
}

func TestCanReuseRecall(t *testing.T) {
	a := NewContextAssembler(nil, AssemblerConfig{MaxEntries: 10})
	key := "ktest"

	if a.canReuseRecall(key, "anything") {
		t.Fatal("should not reuse when no previous result")
	}

	a.cache.setLastRecall(key, "hello world", []*MemoryEntry{{ID: "e1"}})

	if !a.canReuseRecall(key, "hello world") {
		t.Fatal("should reuse same query within min interval")
	}

	if !a.canReuseRecall(key, "completely different query") {
		t.Fatal("should reuse within min interval regardless of query")
	}

	snap, _ := a.cache.getLastRecall(key)
	snap.at = time.Now().Add(-recallThrottleMinInterval - time.Second)
	a.cache.mu.Lock()
	a.cache.lastRecall[key] = snap
	a.cache.mu.Unlock()

	if !a.canReuseRecall(key, "hello world") {
		t.Fatal("should reuse same query even after min interval")
	}

	if !a.canReuseRecall(key, "hello worl") {
		t.Fatal("should reuse similar query (low change ratio)")
	}

	if a.canReuseRecall(key, "xyz completely different") {
		t.Fatal("should not reuse very different query after min interval")
	}

	snap, _ = a.cache.getLastRecall(key)
	snap.at = time.Now().Add(-recallThrottleMaxInterval - time.Second)
	a.cache.mu.Lock()
	a.cache.lastRecall[key] = snap
	a.cache.mu.Unlock()

	if a.canReuseRecall(key, "hello world") {
		t.Fatal("should not reuse after max interval even with same query")
	}
}
