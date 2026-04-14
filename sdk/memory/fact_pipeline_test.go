package memory

import (
	"context"
	"testing"
)

func TestMemoryEntryFromCandidate_TimelineSessionID(t *testing.T) {
	in := ExtractInput{
		RuntimeID:         "rt1",
		TimelineSessionID: "thread-a",
		Scope:             MemoryScope{UserID: "alice"},
		Source:            MemorySource{ConversationID: "alice", RuntimeID: "rt1"},
	}
	e := memoryEntryFromCandidate(
		CandidateMemory{Category: CategoryEntities, Content: "fact"},
		in.Source, in, nil,
	)
	if e.Scope.SessionID != "thread-a" {
		t.Fatalf("SessionID: got %q want thread-a", e.Scope.SessionID)
	}
	if e.Scope.UserID != "alice" {
		t.Fatalf("UserID: got %q", e.Scope.UserID)
	}
}

func TestMemoryEntryFromCandidate_ConversationIDWhenDiffersFromUser(t *testing.T) {
	in := ExtractInput{
		RuntimeID: "rt1",
		Scope:     MemoryScope{UserID: "owner-1"},
		Source:    MemorySource{ConversationID: "conv-77", RuntimeID: "rt1"},
	}
	e := memoryEntryFromCandidate(
		CandidateMemory{Category: CategoryEntities, Content: "entity"},
		in.Source, in, nil,
	)
	if e.Scope.SessionID != "conv-77" {
		t.Fatalf("SessionID fallback: got %q want conv-77 (use non-global category)", e.Scope.SessionID)
	}
}

func TestMemoryEntryFromCandidate_SkipsSessionWhenConvEqualsUser(t *testing.T) {
	in := ExtractInput{
		RuntimeID: "rt1",
		Scope:     MemoryScope{UserID: "same-user"},
		Source:    MemorySource{ConversationID: "same-user", RuntimeID: "rt1"},
	}
	e := memoryEntryFromCandidate(
		CandidateMemory{Category: CategoryEvents, Content: "event note"},
		in.Source, in, nil,
	)
	if e.Scope.SessionID != "" {
		t.Fatalf("expected empty SessionID when ConversationID equals UserID, got %q", e.Scope.SessionID)
	}
}

func TestMemoryEntryFromCandidate_GlobalCategoryClearsScope(t *testing.T) {
	in := ExtractInput{
		RuntimeID:         "rt1",
		TimelineSessionID: "should-not-appear-on-global-row",
		Scope:             MemoryScope{UserID: "u1"},
		Source:            MemorySource{RuntimeID: "rt1"},
	}
	// profile is in DefaultGlobalCategories
	e := memoryEntryFromCandidate(
		CandidateMemory{Category: CategoryProfile, Content: "name"},
		in.Source, in, nil,
	)
	if !e.Scope.IsGlobal() {
		t.Fatalf("expected global scope, got %+v", e.Scope)
	}
}

func TestSearchStage_StripSessionIDFromSearch(t *testing.T) {
	u1s1 := MemoryScope{UserID: "u1", SessionID: "s1", RuntimeID: "r1"}
	spy := &scopeSpyLTStore{}
	stage := &searchStage{
		store:    spy,
		ltConfig: LongTermConfig{Enabled: true, ScopeEnabled: true},
	}
	state := &PipelineState{
		Input: ExtractInput{
			RuntimeID:                "r1",
			StripSessionIDFromSearch: true,
			Scope:                    u1s1,
			Messages:                 nil,
		},
		Candidates: []CandidateMemory{{Category: CategoryEntities, Content: "query text"}},
	}
	if err := stage.Run(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if spy.lastSearchScope == nil {
		t.Fatal("expected Search to be called")
	}
	if spy.lastSearchScope.SessionID != "" {
		t.Fatalf("StripSessionIDFromSearch: got SessionID %q", spy.lastSearchScope.SessionID)
	}
	if spy.lastSearchScope.UserID != "u1" {
		t.Fatalf("UserID: got %q", spy.lastSearchScope.UserID)
	}
}

func TestSearchStage_NoStripKeepsSessionInSearchScope(t *testing.T) {
	u1s1 := MemoryScope{UserID: "u1", SessionID: "s1", RuntimeID: "r1"}
	spy := &scopeSpyLTStore{}
	stage := &searchStage{store: spy, ltConfig: LongTermConfig{Enabled: true, ScopeEnabled: true}}
	state := &PipelineState{
		Input: ExtractInput{
			RuntimeID:                "r1",
			StripSessionIDFromSearch: false,
			Scope:                    u1s1,
		},
		Candidates: []CandidateMemory{{Category: CategoryEntities, Content: "q"}},
	}
	if err := stage.Run(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if spy.lastSearchScope == nil || spy.lastSearchScope.SessionID != "s1" {
		t.Fatalf("expected SessionID s1 in search scope, got %+v", spy.lastSearchScope)
	}
}

type scopeSpyLTStore struct {
	lastSearchScope *MemoryScope
}

func (s *scopeSpyLTStore) Save(context.Context, string, *MemoryEntry) error { return nil }
func (s *scopeSpyLTStore) List(context.Context, string, ListOptions) ([]*MemoryEntry, error) {
	return nil, nil
}
func (s *scopeSpyLTStore) Search(_ context.Context, _ string, _ string, opts SearchOptions) ([]*MemoryEntry, error) {
	if opts.Scope != nil {
		tmp := *opts.Scope
		s.lastSearchScope = &tmp
	} else {
		s.lastSearchScope = nil
	}
	return nil, nil
}
func (s *scopeSpyLTStore) Update(context.Context, string, *MemoryEntry) error { return nil }
func (s *scopeSpyLTStore) Delete(context.Context, string, string) error       { return nil }

type mergeScopeCaptureStore struct {
	updated *MemoryEntry
}

func (m *mergeScopeCaptureStore) Save(context.Context, string, *MemoryEntry) error { return nil }
func (m *mergeScopeCaptureStore) List(context.Context, string, ListOptions) ([]*MemoryEntry, error) {
	return nil, nil
}
func (m *mergeScopeCaptureStore) Search(context.Context, string, string, SearchOptions) ([]*MemoryEntry, error) {
	return nil, nil
}
func (m *mergeScopeCaptureStore) Update(_ context.Context, _ string, entry *MemoryEntry) error {
	tmp := *entry
	m.updated = &tmp
	return nil
}
func (m *mergeScopeCaptureStore) Delete(context.Context, string, string) error { return nil }

func TestPersistStage_Merge_UsesCurrentTimelineSessionID(t *testing.T) {
	store := &mergeScopeCaptureStore{}
	stage := &persistStage{store: store, ltConfig: LongTermConfig{Enabled: true, ScopeEnabled: true}}

	ex := &MemoryEntry{
		ID:       "lt-row-1",
		Category: CategoryEntities,
		Content:  "alpha content",
		Scope: MemoryScope{
			RuntimeID: "rt",
			UserID:    "u1",
			SessionID: "session-alpha",
		},
	}
	cand := CandidateMemory{Category: CategoryEntities, Content: "beta note"}
	in := ExtractInput{
		RuntimeID:                "rt",
		TimelineSessionID:        "session-beta",
		StripSessionIDFromSearch: true,
		Scope:                    MemoryScope{RuntimeID: "rt", UserID: "u1"},
		Source:                   MemorySource{ConversationID: "u1", RuntimeID: "rt"},
		Messages:                 nil,
	}
	state := &PipelineState{
		Input: in,
		ToDedup: []dedupItem{{
			cand:     cand,
			existing: []*MemoryEntry{ex},
		}},
		Actions: []deduplicationResult{{
			Action:        ActionMerge,
			TargetID:      "lt-row-1",
			MergedContent: "merged from beta",
		}},
		DedupFallbackCreateAll: false,
	}
	if err := stage.Run(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	if store.updated == nil {
		t.Fatal("expected Update call")
	}
	if store.updated.Scope.SessionID != "session-beta" {
		t.Fatalf("merge row session_id: got %q want session-beta", store.updated.Scope.SessionID)
	}
	if store.updated.Scope.UserID != "u1" {
		t.Fatalf("UserID: got %q", store.updated.Scope.UserID)
	}
}
