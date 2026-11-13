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

// All is the catalog in discovery order, which is what an unfiltered listing
// returns: the maps behind the index cannot provide a stable sequence.
func TestIndexAllIsDiscoveryOrder(t *testing.T) {
	// A duplicate path is impossible past the index (bm25 rejects the id), so the
	// catalog order here is the discovery order the caller produced.
	skills := []Metadata{
		{Name: "b", Path: "/s/b/SKILL.md", Description: "second"},
		{Name: "a", Path: "/s/a/SKILL.md", Description: "first"},
	}
	index, err := NewIndex(skills)
	if err != nil {
		t.Fatalf("NewIndex: %v", err)
	}

	all := index.All()
	if len(all) != 2 {
		t.Fatalf("All returned %d entries, want 2", len(all))
	}
	if all[0].Name != "b" || all[1].Name != "a" {
		t.Fatalf("All = %v, want discovery order b, a", []string{all[0].Name, all[1].Name})
	}
	if index.Len() != len(all) {
		t.Fatalf("Len = %d, All = %d entries", index.Len(), len(all))
	}
	// A caller cannot reorder what the index reports.
	all[0].Name = "mutated"
	if again := index.All(); again[0].Name != "b" {
		t.Fatalf("All returned the index's own slice: %q", again[0].Name)
	}
	if empty, _ := NewIndex(nil); empty.All() != nil {
		t.Fatal("an empty catalog returned a non-nil All")
	}
}
