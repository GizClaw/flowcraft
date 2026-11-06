package skill

import (
	"testing"
)

func skills() []Metadata {
	return []Metadata{
		{Name: "alpha", Description: "does the alpha thing", Path: "/skills/alpha/SKILL.md"},
		{Name: "beta", Description: "handles beta payloads", Path: "/skills/beta/SKILL.md"},
	}
}

func TestIndexRanksAndResolves(t *testing.T) {
	index, err := NewIndex(skills())
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	ranked := index.Rank("alpha thing", 5, 0)
	if len(ranked) == 0 || ranked[0].Name != "alpha" {
		t.Errorf("Rank = %+v, want alpha first", ranked)
	}
	if got := index.Rank("   ", 5, 0); got != nil {
		t.Errorf("Rank(empty) = %v, want nil", got)
	}
	if skill, ok := index.ByName("beta"); !ok || skill.Path != "/skills/beta/SKILL.md" {
		t.Errorf("ByName(beta) = %+v, %v", skill, ok)
	}
}

func TestMentioned(t *testing.T) {
	index, err := NewIndex(skills())
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}
	got := index.Mentioned("please use $alpha and then $beta, not $500")
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Errorf("Mentioned = %+v, want alpha then beta", got)
	}
	if again := index.Mentioned("$alpha $alpha"); len(again) != 1 {
		t.Errorf("Mentioned kept duplicates: %+v", again)
	}
	if none := index.Mentioned("$500"); len(none) != 0 {
		t.Errorf("Mentioned($500) = %+v, want none", none)
	}
}
