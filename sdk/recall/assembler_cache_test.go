package recall

import (
	"testing"
	"time"
)

func TestAssemblerCache_PinnedEvictOnMax(t *testing.T) {
	c := newAssemblerCache()
	defer c.close()
	c.maxEntries = 2

	c.writePinned("a", []*MemoryEntry{{Content: "a"}}, time.Hour)
	c.writePinned("b", []*MemoryEntry{{Content: "b"}}, 2*time.Hour)
	// Should evict "a" (earlier expiration)
	c.writePinned("c", []*MemoryEntry{{Content: "c"}}, time.Hour)

	if _, ok := c.readPinned("a"); ok {
		t.Fatal("expected 'a' to be evicted")
	}
	if _, ok := c.readPinned("b"); !ok {
		t.Fatal("expected 'b' to remain")
	}
	if _, ok := c.readPinned("c"); !ok {
		t.Fatal("expected 'c' to remain")
	}
}

func TestAssemblerCache_RecallEvictOnMax(t *testing.T) {
	c := newAssemblerCache()
	defer c.close()
	c.maxEntries = 2

	c.writeRecall("x", []*MemoryEntry{{Content: "x"}}, time.Hour)
	c.writeRecall("y", []*MemoryEntry{{Content: "y"}}, 2*time.Hour)
	c.writeRecall("z", []*MemoryEntry{{Content: "z"}}, time.Hour)

	if _, ok := c.readRecall("x"); ok {
		t.Fatal("expected 'x' to be evicted")
	}
	if _, ok := c.readRecall("y"); !ok {
		t.Fatal("expected 'y' to remain")
	}
	if _, ok := c.readRecall("z"); !ok {
		t.Fatal("expected 'z' to remain")
	}
}

func TestAssemblerCache_LastRecallEvictOnMax(t *testing.T) {
	c := newAssemblerCache()
	defer c.close()
	c.maxEntries = 2

	c.setLastRecall("s1", "q1", []*MemoryEntry{{Content: "1"}})
	time.Sleep(time.Millisecond)
	c.setLastRecall("s2", "q2", []*MemoryEntry{{Content: "2"}})
	time.Sleep(time.Millisecond)
	c.setLastRecall("s3", "q3", []*MemoryEntry{{Content: "3"}})

	if _, ok := c.getLastRecall("s1"); ok {
		t.Fatal("expected 's1' to be evicted")
	}
	if _, ok := c.getLastRecall("s2"); !ok {
		t.Fatal("expected 's2' to remain")
	}
	if _, ok := c.getLastRecall("s3"); !ok {
		t.Fatal("expected 's3' to remain")
	}
}

func TestAssemblerCache_EvictExpired(t *testing.T) {
	c := newAssemblerCache()
	defer c.close()

	c.writePinned("expired", []*MemoryEntry{{Content: "old"}}, time.Millisecond)
	c.writePinned("fresh", []*MemoryEntry{{Content: "new"}}, time.Hour)
	c.writeRecall("expired_r", []*MemoryEntry{{Content: "old"}}, time.Millisecond)

	time.Sleep(5 * time.Millisecond)
	c.evictExpired()

	c.mu.RLock()
	pinnedLen := len(c.pinned)
	recallLen := len(c.recall)
	c.mu.RUnlock()

	if pinnedLen != 1 {
		t.Fatalf("expected 1 pinned after evict, got %d", pinnedLen)
	}
	if recallLen != 0 {
		t.Fatalf("expected 0 recall after evict, got %d", recallLen)
	}
}

func TestAssemblerCache_Close(t *testing.T) {
	c := newAssemblerCache()
	c.close()
	// Double close should not panic.
	c.close()
}
