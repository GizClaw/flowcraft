package recalltest

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/memory/recall"
)

func conformanceScope() recall.Scope {
	return recall.Scope{RuntimeID: "rt", UserID: "u1"}
}

func gotIDs(facts []recall.TemporalFact) string {
	ids := make([]string, 0, len(facts))
	for _, f := range facts {
		ids = append(ids, f.ID)
	}
	return strings.Join(ids, ",")
}

func scopeEnumeratorForTest(t testing.TB, newStore ScopeEnumeratorFactory) (recall.TemporalStore, recall.ScopeEnumerator) {
	t.Helper()
	store, enum := newStore(t)
	if store == nil {
		t.Fatal("nil TemporalStore")
	}
	if enum == nil {
		t.Fatal("nil ScopeEnumerator")
	}
	return store, enum
}

func gotScopeKeys(scopes []recall.Scope) string {
	keys := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		keys = append(keys, scope.PartitionKey())
	}
	return strings.Join(keys, ",")
}
