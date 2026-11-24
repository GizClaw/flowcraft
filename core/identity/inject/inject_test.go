package inject_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/identity"
	"github.com/GizClaw/flowcraft/core/identity/inject"
)

func session() identity.Session {
	return identity.Session{
		Tenant: "tenant-1", Agent: "assistant", SessionID: "session-1",
		Authority: "authority-1", Bind: "bind-1",
	}
}

func withSession(t *testing.T) context.Context {
	t.Helper()
	ctx, err := inject.Session(context.Background(), session())
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	return ctx
}

func TestSessionTurnChain(t *testing.T) {
	ctx := withSession(t)
	ctx, err := inject.Turn(ctx, identity.Turn{TaskID: "task-1", Ordinal: 1})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	ctx, err = inject.Chain(ctx, identity.Chain{ID: "chain-1", Depth: 1})
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	run, ok := identity.Lookup(ctx)
	if !ok {
		t.Fatal("Lookup found nothing")
	}
	if run.SessionID != "session-1" || run.Bind != "bind-1" || run.TaskID != "task-1" ||
		run.Ordinal != 1 || run.ChainID != "chain-1" || run.Depth != 1 {
		t.Fatalf("run = %+v", run)
	}
}

// TestSessionRequiresTheFactsItPromises keeps a half-identified session out of the
// trace: the agent and the session id are what every consumer filters on.
func TestSessionRequiresTheFactsItPromises(t *testing.T) {
	for name, session := range map[string]identity.Session{
		"no agent":      {SessionID: "session-1"},
		"no session id": {Agent: "assistant"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := inject.Session(context.Background(), session); err == nil {
				t.Fatalf("Session accepted %+v", session)
			}
		})
	}
}

// TestASecondInjectionIsAnError is the rule the whole package exists for: one trace
// carries one identity, and merging field by field is how two spans stop agreeing
// about which session they belong to.
func TestASecondInjectionIsAnError(t *testing.T) {
	ctx := withSession(t)
	if _, err := inject.Session(ctx, session()); err == nil {
		t.Fatal("a second Session was accepted")
	}

	turned, err := inject.Turn(ctx, identity.Turn{TaskID: "task-1", Ordinal: 1})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if _, err := inject.Turn(turned, identity.Turn{TaskID: "task-2", Ordinal: 2}); err == nil {
		t.Fatal("a second Turn was accepted")
	}

	chained, err := inject.Chain(ctx, identity.Chain{ID: "chain-1"})
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if _, err := inject.Chain(chained, identity.Chain{ID: "chain-2"}); err == nil {
		t.Fatal("a second Chain was accepted")
	}
}

// TestTurnAndChainRequireASession keeps an orphan fact from being attached to a
// conversation nobody named: a turn without a session has nowhere to belong.
func TestTurnAndChainRequireASession(t *testing.T) {
	bare := context.Background()
	if _, err := inject.Turn(bare, identity.Turn{Ordinal: 1}); err == nil {
		t.Fatal("Turn was accepted without a session")
	}
	if _, err := inject.Chain(bare, identity.Chain{ID: "chain-1"}); err == nil {
		t.Fatal("Chain was accepted without a session")
	}
}

func TestTurnAndChainValidateTheirOwnFacts(t *testing.T) {
	ctx := withSession(t)
	if _, err := inject.Turn(ctx, identity.Turn{Ordinal: 0}); err == nil {
		t.Fatal("a turn without an ordinal was accepted")
	}
	if _, err := inject.Chain(ctx, identity.Chain{}); err == nil {
		t.Fatal("a chain without an id was accepted")
	}
	if _, err := inject.Chain(ctx, identity.Chain{ID: "chain-1", Depth: -1}); err == nil {
		t.Fatal("a negative depth was accepted")
	}
}

// TestInjectDoesNotDisturbOtherFacts pins the merge: attaching a turn keeps the
// session, and attaching a chain keeps both.
func TestInjectDoesNotDisturbOtherFacts(t *testing.T) {
	ctx := withSession(t)
	sessionBefore, _ := identity.Lookup(ctx)

	turned, err := inject.Turn(ctx, identity.Turn{TaskID: "task-1", Ordinal: 2})
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	afterTurn, _ := identity.Lookup(turned)
	if afterTurn.SessionID != sessionBefore.SessionID || afterTurn.Bind != sessionBefore.Bind {
		t.Fatalf("Turn dropped the session: %+v", afterTurn)
	}

	chained, err := inject.Chain(turned, identity.Chain{ID: "chain-1", Depth: 2})
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	afterChain, _ := identity.Lookup(chained)
	if afterChain.TaskID != "task-1" || afterChain.Ordinal != 2 {
		t.Fatalf("Chain dropped the turn: %+v", afterChain)
	}
	// The parent context is unaffected: each injection derives a new one.
	unchanged, _ := identity.Lookup(ctx)
	if unchanged.Ordinal != 0 || unchanged.ChainID != "" {
		t.Fatalf("injection mutated the parent context: %+v", unchanged)
	}
}

func TestErrorsNameTheMistake(t *testing.T) {
	_, err := inject.Turn(context.Background(), identity.Turn{Ordinal: 1})
	if err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("err = %v, want it to name the missing session", err)
	}
}
