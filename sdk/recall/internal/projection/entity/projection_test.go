package entity

import (
	"context"
	"sort"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

func TestProject_Lookup(t *testing.T) {
	p := New()
	scope := domain.Scope{RuntimeID: "rt"}
	facts := []domain.TemporalFact{
		{ID: "f1", Scope: scope, Kind: domain.KindRelation, Entities: []string{"alice"}, Subject: "alice", Object: "bob"},
		{ID: "f2", Scope: scope, Kind: domain.KindNote, Entities: []string{"alice", "carol"}},
		{ID: "f3", Scope: scope, Kind: domain.KindNote, Entities: []string{"bob"}},
	}
	if err := p.Project(context.Background(), facts); err != nil {
		t.Fatalf("project: %v", err)
	}
	got := p.Lookup(context.Background(), scope, []string{"alice"})
	sort.Strings(got)
	want := []string{"f1", "f2"}
	if !equalStrings(got, want) {
		t.Errorf("alice lookup = %v, want %v", got, want)
	}
	bob := p.Lookup(context.Background(), scope, []string{"BOB"})
	sort.Strings(bob)
	if !equalStrings(bob, []string{"f1", "f3"}) {
		t.Errorf("case-insensitive bob lookup = %v", bob)
	}
}

func TestProject_ReplacesPriorMentions(t *testing.T) {
	p := New()
	scope := domain.Scope{RuntimeID: "rt"}
	if err := p.Project(context.Background(), []domain.TemporalFact{
		{ID: "f1", Scope: scope, Kind: domain.KindNote, Entities: []string{"alice"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Project(context.Background(), []domain.TemporalFact{
		{ID: "f1", Scope: scope, Kind: domain.KindNote, Entities: []string{"bob"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(context.Background(), scope, []string{"alice"}); len(got) != 0 {
		t.Errorf("stale alice posting not cleaned: %v", got)
	}
	if got := p.Lookup(context.Background(), scope, []string{"bob"}); !equalStrings(got, []string{"f1"}) {
		t.Errorf("bob lookup = %v", got)
	}
}

func TestForget_RemovesPostings(t *testing.T) {
	p := New()
	scope := domain.Scope{RuntimeID: "rt"}
	_ = p.Project(context.Background(), []domain.TemporalFact{
		{ID: "f1", Scope: scope, Kind: domain.KindNote, Entities: []string{"alice"}},
		{ID: "f2", Scope: scope, Kind: domain.KindNote, Entities: []string{"alice"}},
	})
	if err := p.Forget(context.Background(), scope, []string{"f1"}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(context.Background(), scope, []string{"alice"}); !equalStrings(got, []string{"f2"}) {
		t.Errorf("alice lookup after forget = %v", got)
	}
}

func TestRebuild_ResetsScope(t *testing.T) {
	p := New()
	scope := domain.Scope{RuntimeID: "rt"}
	_ = p.Project(context.Background(), []domain.TemporalFact{
		{ID: "f1", Scope: scope, Kind: domain.KindNote, Entities: []string{"alice"}},
	})
	if err := p.Rebuild(context.Background(), scope, []domain.TemporalFact{
		{ID: "f2", Scope: scope, Kind: domain.KindNote, Entities: []string{"bob"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := p.Lookup(context.Background(), scope, []string{"alice"}); len(got) != 0 {
		t.Errorf("alice should be gone after rebuild: %v", got)
	}
	if got := p.Lookup(context.Background(), scope, []string{"bob"}); !equalStrings(got, []string{"f2"}) {
		t.Errorf("bob lookup after rebuild = %v", got)
	}
}

func TestScopeIsolation(t *testing.T) {
	p := New()
	a := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	b := domain.Scope{RuntimeID: "rt", UserID: "u2"}
	_ = p.Project(context.Background(), []domain.TemporalFact{
		{ID: "f1", Scope: a, Kind: domain.KindNote, Entities: []string{"alice"}},
		{ID: "f2", Scope: b, Kind: domain.KindNote, Entities: []string{"alice"}},
	})
	if got := p.Lookup(context.Background(), a, []string{"alice"}); !equalStrings(got, []string{"f1"}) {
		t.Errorf("scope a lookup = %v", got)
	}
	if got := p.Lookup(context.Background(), b, []string{"alice"}); !equalStrings(got, []string{"f2"}) {
		t.Errorf("scope b lookup = %v", got)
	}
}

func TestAgentIDIsSoftIsolation(t *testing.T) {
	p := New()
	a := domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-a"}
	b := domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "agent-b"}
	_ = p.Project(context.Background(), []domain.TemporalFact{
		{ID: "f1", Scope: a, Kind: domain.KindNote, Entities: []string{"alice"}},
		{ID: "f2", Scope: b, Kind: domain.KindNote, Entities: []string{"alice"}},
	})
	if got := p.Lookup(context.Background(), a, []string{"alice"}); !equalStrings(got, []string{"f1", "f2"}) {
		t.Fatalf("AgentID is soft isolation metadata and must not partition entity projection, got %v", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
