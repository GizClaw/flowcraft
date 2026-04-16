package bindings

import (
	"container/list"
	"fmt"
	"testing"
)

func TestExprProgramCache_evictionLRU(t *testing.T) {
	c := &exprProgramCache{
		max:   4,
		ll:    list.New(),
		items: make(map[string]*list.Element),
	}

	keys := []string{"n+0", "n+1", "n+2", "n+3"}
	for _, k := range keys {
		if _, err := c.getOrCompile(k); err != nil {
			t.Fatalf("compile %q: %v", k, err)
		}
	}
	// Order (MRU..LRU): n+3, n+2, n+1, n+0 — back is n+0
	if _, err := c.getOrCompile("n+0"); err != nil {
		t.Fatal(err)
	}
	// Touch n+0 → MRU; LRU tail should be n+1
	if _, err := c.getOrCompile("n+4"); err != nil {
		t.Fatal(err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.items) != 4 {
		t.Fatalf("want 4 entries, got %d", len(c.items))
	}
	if _, ok := c.items["n+1"]; ok {
		t.Fatal("expected n+1 (LRU) to be evicted")
	}
	for _, k := range []string{"n+0", "n+2", "n+3", "n+4"} {
		if _, ok := c.items[k]; !ok {
			t.Fatalf("missing key %q", k)
		}
	}
}

func TestExprProgramCache_maxBound(t *testing.T) {
	c := &exprProgramCache{
		max:   4,
		ll:    list.New(),
		items: make(map[string]*list.Element),
	}
	for i := 0; i < 6; i++ {
		if _, err := c.getOrCompile(fmt.Sprintf("n + %d", i)); err != nil {
			t.Fatalf("compile %d: %v", i, err)
		}
	}
	c.mu.Lock()
	n := len(c.items)
	c.mu.Unlock()
	if n != 4 {
		t.Fatalf("want 4 cached programs after eviction, got %d", n)
	}
}
