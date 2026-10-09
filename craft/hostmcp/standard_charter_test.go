package hostmcp

import (
	"context"
	"sort"
	"testing"
)

// TestCharterMatchesStandard scans the primitive table against the
// tools Standard knows, in both directions: an unbound service must
// yield exactly the table's tools in host_about's denied list.
func TestCharterMatchesStandard(t *testing.T) {
	t.Parallel()
	registry := NewRegistry()
	if err := Standard(registry, Services{}, "0.1.0"); err != nil {
		t.Fatalf("Standard: %v", err)
	}
	if tools := registry.Tools(); len(tools) != 1 || tools[0].Name != "host_about" {
		t.Fatalf("unbound standard registered %v, want only host_about", tools)
	}
	about, _ := registry.Lookup("host_about")
	value, err := about.Handler(context.Background(), Call{
		Identity: Identity{PluginID: "hello"},
	})
	if err != nil {
		t.Fatalf("host_about: %v", err)
	}
	denied := make([]string, 0, len(Charter))
	for _, entry := range value.(About).Denied {
		if entry.Reason == "no_service" {
			denied = append(denied, entry.Tool)
		}
	}
	sort.Strings(denied)
	want := make([]string, 0, len(Charter))
	for _, row := range Charter {
		if row.Tool != "host_about" {
			want = append(want, row.Tool)
		}
	}
	sort.Strings(want)
	if len(denied) != len(want) {
		t.Fatalf("denied = %v, want %v", denied, want)
	}
	for i := range want {
		if denied[i] != want[i] {
			t.Fatalf("denied = %v, want %v", denied, want)
		}
	}
}
