package recall

import "testing"

func TestNormalizePartitions_EmptyReturnsNil(t *testing.T) {
	if got := NormalizePartitions(nil); got != nil {
		t.Fatalf("nil input: got %v", got)
	}
	if got := NormalizePartitions([]Partition{}); got != nil {
		t.Fatalf("empty slice: got %v", got)
	}
}

func TestNormalizePartitions_Dedupe(t *testing.T) {
	got := NormalizePartitions([]Partition{PartitionUser, PartitionGlobal, PartitionUser, ""})
	if len(got) != 2 || got[0] != PartitionUser || got[1] != PartitionGlobal {
		t.Fatalf("got %v", got)
	}
}

func TestScope_EffectivePartitions_Auto(t *testing.T) {
	if got := (Scope{UserID: "alice"}).EffectivePartitions(); len(got) != 1 || got[0] != PartitionUser {
		t.Fatalf("user-only scope auto-derives PartitionUser: %v", got)
	}
	if got := (Scope{}).EffectivePartitions(); len(got) != 1 || got[0] != PartitionGlobal {
		t.Fatalf("empty scope auto-derives PartitionGlobal: %v", got)
	}
}

func TestScope_EffectivePartitions_Explicit(t *testing.T) {
	s := Scope{UserID: "alice", Partitions: []Partition{PartitionUser, PartitionGlobal}}
	got := s.EffectivePartitions()
	if len(got) != 2 || got[0] != PartitionUser || got[1] != PartitionGlobal {
		t.Fatalf("explicit partitions should win: %v", got)
	}
}

func TestEntryMatchesScope_Union(t *testing.T) {
	scope := &Scope{
		RuntimeID:  "r1",
		UserID:     "alice",
		Partitions: []Partition{PartitionUser, PartitionGlobal},
	}
	if !EntryMatchesScope(&Entry{Scope: Scope{}}, scope) {
		t.Fatal("global row should match union")
	}
	if !EntryMatchesScope(&Entry{Scope: Scope{UserID: "alice"}}, scope) {
		t.Fatal("alice row should match union")
	}
	if EntryMatchesScope(&Entry{Scope: Scope{UserID: "bob"}}, scope) {
		t.Fatal("bob row should not match")
	}
}

func TestEntryMatchesScope_DefaultUserOnly(t *testing.T) {
	scope := &Scope{RuntimeID: "r1", UserID: "alice"}
	if !EntryMatchesScope(&Entry{Scope: Scope{UserID: "alice"}}, scope) {
		t.Fatal("alice row should match")
	}
	if EntryMatchesScope(&Entry{Scope: Scope{}}, scope) {
		t.Fatal("global row should not match a non-global scope by default")
	}
}

func TestEntryMatchesScope_DefaultGlobalOnly(t *testing.T) {
	scope := &Scope{RuntimeID: "r1"}
	if !EntryMatchesScope(&Entry{Scope: Scope{}}, scope) {
		t.Fatal("global row should match")
	}
	if EntryMatchesScope(&Entry{Scope: Scope{UserID: "alice"}}, scope) {
		t.Fatal("alice row should not match a global-only scope")
	}
}

func TestEntryMatchesScope_NilMatchesAll(t *testing.T) {
	if !EntryMatchesScope(&Entry{}, nil) {
		t.Fatal("nil query should match")
	}
}
