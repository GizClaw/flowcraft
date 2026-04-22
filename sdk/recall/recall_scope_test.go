package recall

import (
	"testing"
)

func TestNormalizePartitions_DefaultUser(t *testing.T) {
	got := NormalizePartitions(nil)
	if len(got) != 1 || got[0] != PartitionUser {
		t.Fatalf("got %v", got)
	}
	got = NormalizePartitions([]MemoryPartition{})
	if len(got) != 1 || got[0] != PartitionUser {
		t.Fatalf("empty slice: got %v", got)
	}
}

func TestNormalizePartitions_Dedupe(t *testing.T) {
	got := NormalizePartitions([]MemoryPartition{PartitionUser, PartitionGlobal, PartitionUser})
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}

func TestEntryMatchesRecallScope_Union(t *testing.T) {
	rec := &RecallScope{
		RuntimeID:  "r1",
		UserID:     "alice",
		Partitions: []MemoryPartition{PartitionUser, PartitionGlobal},
	}
	globalEntry := &MemoryEntry{Scope: MemoryScope{}}
	if !EntryMatchesRecallScope(globalEntry, rec) {
		t.Fatal("global row should match union")
	}
	aliceEntry := &MemoryEntry{Scope: MemoryScope{UserID: "alice"}}
	if !EntryMatchesRecallScope(aliceEntry, rec) {
		t.Fatal("alice row should match union")
	}
	bobEntry := &MemoryEntry{Scope: MemoryScope{UserID: "bob"}}
	if EntryMatchesRecallScope(bobEntry, rec) {
		t.Fatal("bob row should not match")
	}
}

func TestEffectiveRecallForSearch_PrefersRecall(t *testing.T) {
	explicit := &RecallScope{Partitions: []MemoryPartition{PartitionGlobal}}
	opts := SearchOptions{
		Scope:  &MemoryScope{UserID: "alice"},
		Recall: explicit,
	}
	got := EffectiveRecallForSearch(opts, "rt")
	if got == nil || len(got.Partitions) != 1 || got.Partitions[0] != PartitionGlobal {
		t.Fatalf("Recall should win: %+v", got)
	}
}

func TestEffectiveRecallForSearch_DerivesFromScope(t *testing.T) {
	opts := SearchOptions{Scope: &MemoryScope{UserID: "u1", SessionID: "s1"}}
	got := EffectiveRecallForSearch(opts, "rt")
	if got == nil || got.UserID != "u1" || got.SessionID != "s1" {
		t.Fatalf("got %+v", got)
	}
	if len(got.Partitions) != 1 || got.Partitions[0] != PartitionUser {
		t.Fatalf("partitions %v", got.Partitions)
	}
}

func TestEffectiveRecallForSearch_GlobalScope(t *testing.T) {
	opts := SearchOptions{Scope: &MemoryScope{RuntimeID: "rt"}}
	got := EffectiveRecallForSearch(opts, "rt")
	if got == nil || len(got.Partitions) != 1 || got.Partitions[0] != PartitionGlobal {
		t.Fatalf("global MemoryScope should derive global partition: %+v", got)
	}
}
