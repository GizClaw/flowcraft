package ingest

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall/internal/domain"
)

func TestStaticAliasResolver_LookupCaseInsensitive(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt", UserID: "u1"}
	r := NewStaticAliasResolver(ScopeAliasEntry{
		Scope: scope, Aliases: map[string]string{"Rowan": "robert", "avery": "Avery Linden"},
	})
	if got := r.Canonical(scope, "rowan"); got != "robert" {
		t.Errorf("lowercase lookup = %q", got)
	}
	if got := r.Canonical(scope, "ROWAN"); got != "robert" {
		t.Errorf("uppercase lookup = %q", got)
	}
	if got := r.Canonical(scope, "Avery"); got != "Avery Linden" {
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
		ScopeAliasEntry{Scope: runtimeScope, Aliases: map[string]string{"rowan": "global-rowan"}},
		ScopeAliasEntry{Scope: userScope, Aliases: map[string]string{"rowan": "user-rowan"}},
		ScopeAliasEntry{Scope: agentScope, Aliases: map[string]string{"rowan": "agent-rowan"}},
	)

	if got := r.Canonical(agentScope, "rowan"); got != "agent-rowan" {
		t.Errorf("agent wins: got %q", got)
	}
	if got := r.Canonical(userScope, "rowan"); got != "user-rowan" {
		t.Errorf("user fallback: got %q", got)
	}
	if got := r.Canonical(runtimeScope, "rowan"); got != "global-rowan" {
		t.Errorf("runtime fallback: got %q", got)
	}

	// agent scope with a different user falls back to runtime.
	otherUser := domain.Scope{RuntimeID: "rt", UserID: "u2", AgentID: "a9"}
	if got := r.Canonical(otherUser, "rowan"); got != "global-rowan" {
		t.Errorf("cross-user fallback: got %q", got)
	}
}

func TestAliasEntityResolver_AppliesAliasAndDedupes(t *testing.T) {
	scope := domain.Scope{RuntimeID: "rt"}
	alias := NewStaticAliasResolver(ScopeAliasEntry{
		Scope: scope, Aliases: map[string]string{"Rowan": "robert"},
	})
	er := newAliasEntityResolver(alias)
	out := er.Resolve(domain.TemporalFact{
		Scope:    scope,
		Subject:  "Rowan",
		Object:   "Avery",
		Entities: []string{"Rowan", "avery"},
	})
	if out.Subject != "robert" {
		t.Errorf("subject not aliased: %q", out.Subject)
	}
	if out.Object != "Avery" {
		t.Errorf("object preserved when no alias: %q", out.Object)
	}
	wantEntities := map[string]bool{"robert": true, "avery": true}
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
		Subject:  "Rowan",
		Entities: []string{"Rowan"},
	})
	if out.Subject != "Rowan" {
		t.Errorf("nil alias should leave subject untouched, got %q", out.Subject)
	}
	if len(out.Entities) != 1 || out.Entities[0] != "rowan" {
		t.Errorf("entities = %v", out.Entities)
	}
}
