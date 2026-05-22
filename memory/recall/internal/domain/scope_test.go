package domain

import "testing"

func TestScope_EffectiveFederation(t *testing.T) {
	primary := Scope{RuntimeID: "rt", UserID: "alice"}
	got := primary.EffectiveFederation()
	if len(got) != 1 || got[0].UserID != "alice" {
		t.Fatalf("nil federation = primary only: %+v", got)
	}

	emptyFed := Scope{RuntimeID: "rt", UserID: "alice", Federation: []Scope{}}
	if len(emptyFed.EffectiveFederation()) != 1 {
		t.Fatal("empty slice federation should equal nil")
	}

	multi := Scope{
		RuntimeID: "rt",
		UserID:    "alice",
		Federation: []Scope{
			{RuntimeID: "rt"},
			{RuntimeID: "rt", UserID: "alice"},
		},
	}
	got = multi.EffectiveFederation()
	if len(got) != 2 {
		t.Fatalf("want primary+global, got %d scopes", len(got))
	}
	if got[0].UserID != "alice" || got[1].UserID != "" {
		t.Fatalf("order/dedup wrong: %+v", got)
	}
}

func TestScope_CanonicalKey(t *testing.T) {
	if got := (Scope{RuntimeID: "rt", UserID: "alice"}).CanonicalKey(); got != "rt/u:alice" {
		t.Fatalf("user key = %q", got)
	}
	if got := (Scope{RuntimeID: "rt"}).CanonicalKey(); got != "rt/global" {
		t.Fatalf("global key = %q", got)
	}
	agent := Scope{RuntimeID: "rt", UserID: "alice", AgentID: "bot-a"}
	if got := agent.PartitionKey(); got != "rt/u:alice" {
		t.Fatalf("partition key = %q", got)
	}
	if got := agent.CanonicalKey(); got != "rt/u:alice/a:bot-a" {
		t.Fatalf("canonical key with agent = %q", got)
	}
}
