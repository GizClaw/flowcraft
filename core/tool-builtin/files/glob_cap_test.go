package files

import (
	"context"
	"path/filepath"
	"testing"
)

// A match cap alone does not bound the walk: a pattern that matches nothing
// still visits the whole tree. The visit cap must stop it and say so.
func TestGlobStopsAtVisitCap(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		name := filepath.Join(root, "f"+string(rune('a'+i))+".txt")
		if err := tree.Write(ctx, name, []byte("x")); err != nil {
			t.Fatalf("Write %s: %v", name, err)
		}
	}
	glob := find(t, fileTools, GlobName)

	old := maxGlobScanned
	maxGlobScanned = 5
	t.Cleanup(func() { maxGlobScanned = old })

	text, err := call(t, glob, map[string]any{"pattern": "**/*.txt", "path": root})
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	out := decode(t, text)
	if truncated, _ := out["truncated"].(bool); !truncated {
		t.Error("truncated = false, want true when the walk hits its cap")
	}
	if matches, _ := out["matches"].([]any); len(matches) > 5 {
		t.Errorf("matches = %d, want at most the 5 entries that were visited", len(matches))
	}
}
