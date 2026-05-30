package recall_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/recall"
)

func TestAllCategories(t *testing.T) {
	cats := recall.AllCategories()
	if len(cats) < 6 {
		t.Fatalf("expected at least 6 categories, got %d", len(cats))
	}
	for _, want := range []recall.Category{
		recall.CategoryProfile, recall.CategoryPreferences, recall.CategoryEntities,
		recall.CategoryEvents, recall.CategoryCases, recall.CategoryPatterns,
	} {
		found := false
		for _, c := range cats {
			if c == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing built-in category %q", want)
		}
	}
}

func TestRegisterCategory(t *testing.T) {
	recall.RegisterCategory("custom_test_cat")
	cats := recall.AllCategories()
	found := false
	for _, c := range cats {
		if c == "custom_test_cat" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected custom_test_cat in recall.AllCategories after recall.RegisterCategory")
	}

	before := len(recall.AllCategories())
	recall.RegisterCategory("custom_test_cat")
	after := len(recall.AllCategories())
	if after != before {
		t.Fatalf("duplicate recall.RegisterCategory changed count: %d -> %d", before, after)
	}
}

func TestAllCategoryStrings(t *testing.T) {
	strs := recall.AllCategoryStrings()
	if len(strs) < 6 {
		t.Fatalf("expected at least 6 category strings, got %d", len(strs))
	}
	found := false
	for _, s := range strs {
		if s == "profile" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected 'profile' in recall.AllCategoryStrings")
	}
}
