package hostmcp

import (
	"sort"
	"testing"
)

// TestCharterToolsIsSorted pins what the helper promises: the table is
// grouped by service, the names it hands out are ordered.
func TestCharterToolsIsSorted(t *testing.T) {
	t.Parallel()
	tools := CharterTools()
	if len(tools) != len(Charter) {
		t.Fatalf("CharterTools = %v, want one name per row (%d)", tools, len(Charter))
	}
	if !sort.StringsAreSorted(tools) {
		t.Fatalf("CharterTools = %v, want sorted", tools)
	}
	want := make([]string, 0, len(Charter))
	for _, row := range Charter {
		want = append(want, row.Tool)
	}
	sort.Strings(want)
	for i := range want {
		if tools[i] != want[i] {
			t.Fatalf("CharterTools = %v, want %v", tools, want)
		}
	}
}
