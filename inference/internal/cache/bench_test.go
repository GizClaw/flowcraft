package cache

import "testing"

// BenchmarkGetHit measures the lookup every adapter makes before doing the work
// the cache exists to skip: one map read and a list move on a full cache.
func BenchmarkGetHit(b *testing.B) {
	c := New[int](1024)
	for i := 0; i < 1024; i++ {
		c.Put(string(rune('a'+i%26))+string(rune('a'+i/26)), i)
	}
	key := "aa"
	if _, ok := c.Get(key); !ok {
		b.Fatalf("the benchmark key is not cached")
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, ok := c.Get(key); !ok {
			b.Fatal("miss")
		}
	}
}

// BenchmarkPutAtCapacity measures the eviction path: inserting into a full cache is
// the steady state of a long-running process.
func BenchmarkPutAtCapacity(b *testing.B) {
	c := New[int](64)
	key := func(i int) string { return string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) }

	b.ReportAllocs()
	i := 0
	for b.Loop() {
		c.Put(key(i), i)
		i++
	}
}

var getSink int

// TestGetDoesNotAllocate keeps the lookup allocation-free: every adapter calls it
// before doing the work the cache exists to skip, so a map read plus a list move is
// the whole budget. It gates through `make test`, not the bench lane.
func TestGetDoesNotAllocate(t *testing.T) {
	c := New[int](64)
	c.Put("k", 1)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("the fixture entry is not cached")
	}

	allocs := testing.AllocsPerRun(1000, func() {
		getSink, _ = c.Get("k")
	})
	if allocs != 0 {
		t.Fatalf("Get allocates %.0f times per lookup", allocs)
	}
}
