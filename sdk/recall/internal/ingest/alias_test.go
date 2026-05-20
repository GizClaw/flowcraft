package ingest

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall/internal/domain"
)

func TestStaticAliasResolver_LookupCaseInsensitive(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	r := NewStaticAliasResolver(ScopeAliasEntry{
		Scope: scope, Aliases: map[string]string{"Bob": "robert", "alice": "Alice Liddell"},
	})
	if got := r.Canonical(scope, "bob"); got != "robert" {
		t.Errorf("lowercase lookup = %q", got)
	}
	if got := r.Canonical(scope, "BOB"); got != "robert" {
		t.Errorf("uppercase lookup = %q", got)
	}
	if got := r.Canonical(scope, "Alice"); got != "Alice Liddell" {
		t.Errorf("mixed-case lookup = %q", got)
	}
	if got := r.Canonical(scope, "carol"); got != "" {
		t.Errorf("unknown alias must return empty, got %q", got)
	}
}

func TestStaticAliasResolver_ScopeFallback(t *testing.T) {
	runtimeScope := domain.Scope{RuntimeID: "rt"}
	userScope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	agentScope := domain.Scope{RuntimeID: "rt", UserID: "u1", AgentID: "a1"}
	r := NewStaticAliasResolver(
		ScopeAliasEntry{Scope: runtimeScope, Aliases: map[string]string{"bob": "global-bob"}},
		ScopeAliasEntry{Scope: userScope, Aliases: map[string]string{"bob": "user-bob"}},
		ScopeAliasEntry{Scope: agentScope, Aliases: map[string]string{"bob": "agent-bob"}},
	)

	if got := r.Canonical(agentScope, "bob"); got != "agent-bob" {
		t.Errorf("agent wins: got %q", got)
	}
	if got := r.Canonical(userScope, "bob"); got != "user-bob" {
		t.Errorf("user fallback: got %q", got)
	}
	if got := r.Canonical(runtimeScope, "bob"); got != "global-bob" {
		t.Errorf("runtime fallback: got %q", got)
	}

	// agent scope with a different user falls back to runtime.
	otherUser := domain.Scope{RuntimeID: "rt", UserID: "u2", AgentID: "a9"}
	if got := r.Canonical(otherUser, "bob"); got != "global-bob" {
		t.Errorf("cross-user fallback: got %q", got)
	}
}

func TestAliasEntityResolver_AppliesAliasAndDedupes(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt"}
	alias := NewStaticAliasResolver(ScopeAliasEntry{
		Scope: scope, Aliases: map[string]string{"Bob": "robert"},
	})
	er := newAliasEntityResolver(alias)
	out := er.Resolve(domain.TemporalFact{
		Scope:    scope,
		Subject:  "Bob",
		Object:   "Alice",
		Entities: []string{"Bob", "alice"},
	})
	if out.Subject != "robert" {
		t.Errorf("subject not aliased: %q", out.Subject)
	}
	if out.Object != "Alice" {
		t.Errorf("object preserved when no alias: %q", out.Object)
	}
	wantEntities := map[string]bool{"robert": true, "alice": true}
	for _, e := range out.Entities {
		delete(wantEntities, e)
	}
	if len(wantEntities) != 0 {
		t.Errorf("entities not as expected, missing %v in %v", wantEntities, out.Entities)
	}
}

func TestAliasEntityResolver_NilAliasFallsBackToNop(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt"}
	er := newAliasEntityResolver(nil)
	out := er.Resolve(domain.TemporalFact{
		Scope:    scope,
		Subject:  "Bob",
		Entities: []string{"Bob"},
	})
	if out.Subject != "Bob" {
		t.Errorf("nil alias should leave subject untouched, got %q", out.Subject)
	}
	if len(out.Entities) != 1 || out.Entities[0] != "bob" {
		t.Errorf("entities = %v", out.Entities)
	}
}
