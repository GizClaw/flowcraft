package cache

import (
	"fmt"
	"sync"
	"testing"
)

func TestLRUEvictsLeastRecentlyUsed(t *testing.T) {
	lru := New[int](2)
	lru.Put("a", 1)
	lru.Put("b", 2)

	// Touch "a" so "b" becomes the least recently used entry.
	if value, ok := lru.Get("a"); !ok || value != 1 {
		t.Fatalf("Get(a) = (%d, %v)", value, ok)
	}
	lru.Put("c", 3)

	if _, ok := lru.Get("b"); ok {
		t.Fatal("the least recently used entry survived eviction")
	}
	for key, want := range map[string]int{"a": 1, "c": 3} {
		if value, ok := lru.Get(key); !ok || value != want {
			t.Fatalf("Get(%q) = (%d, %v), want (%d, true)", key, value, ok, want)
		}
	}
	if got := lru.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
}

func TestLRUReplacesInPlace(t *testing.T) {
	lru := New[string](2)
	lru.Put("a", "first")
	lru.Put("a", "second")
	if got := lru.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1 after replacing a key", got)
	}
	if value, _ := lru.Get("a"); value != "second" {
		t.Fatalf("Get(a) = %q, want the replacement", value)
	}
}

func TestLRUCapacityFloor(t *testing.T) {
	// A misconfigured caller still gets a usable cache rather than a panic.
	lru := New[int](0)
	if got := lru.Capacity(); got != 1 {
		t.Fatalf("Capacity = %d, want 1", got)
	}
	lru.Put("a", 1)
	lru.Put("b", 2)
	if _, ok := lru.Get("a"); ok {
		t.Fatal("capacity one kept two entries")
	}
}

func TestLRUClear(t *testing.T) {
	lru := New[int](4)
	lru.Put("a", 1)
	lru.Clear()
	if got := lru.Len(); got != 0 {
		t.Fatalf("Len = %d after Clear", got)
	}
	if _, ok := lru.Get("a"); ok {
		t.Fatal("Clear left an entry behind")
	}
	// The cache still works after a clear.
	lru.Put("b", 2)
	if value, ok := lru.Get("b"); !ok || value != 2 {
		t.Fatalf("Get(b) after Clear = (%d, %v)", value, ok)
	}
}

// TestLRUConcurrent hammers the cache so the race detector sees the lock discipline.
func TestLRUConcurrent(t *testing.T) {
	lru := New[int](8)
	var workers sync.WaitGroup
	for worker := range 8 {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := range 200 {
				key := fmt.Sprintf("%d-%d", worker, i%16)
				lru.Put(key, i)
				lru.Get(key)
				if i%32 == 0 {
					lru.Len()
				}
			}
		}(worker)
	}
	workers.Wait()
	if got := lru.Len(); got > lru.Capacity() {
		t.Fatalf("Len = %d exceeds Capacity %d", got, lru.Capacity())
	}
}
