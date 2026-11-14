package files

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// max_matches bounds the count; a matched line can be huge, so the returned text
// needs its own budget or a hundred hits is one enormous answer.
func TestGrepStopsAtMatchedByteCap(t *testing.T) {
	fileTools, tree := group(t)
	root := tree.Writable()[0]
	long := strings.Repeat("needle ", 20) // 140 bytes per matching line
	body := strings.Repeat(long+"\n", 10)
	if err := tree.Write(context.Background(), filepath.Join(root, "a.txt"), []byte(body)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	grep := find(t, fileTools, GrepName)

	old := maxGrepMatchedBytes
	maxGrepMatchedBytes = 200
	t.Cleanup(func() { maxGrepMatchedBytes = old })

	text, err := call(t, grep, map[string]any{"pattern": "needle", "path": root, "max_matches": 10})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	out := decode(t, text)
	if truncated, _ := out["truncated"].(bool); !truncated {
		t.Error("truncated = false, want true when matched text hits its cap")
	}
	matches, _ := out["matches"].([]any)
	if len(matches) > 2 {
		t.Errorf("matches = %d, want the 200-byte cap to stop after two lines", len(matches))
	}
}
