package inference

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// TestBindingCacheEvictsLeastRecentlyUsed pins the bound on the key space: a
// host that derives model references dynamically can address many models, so
// the cache evicts the least recently used one — never the models the host
// keeps addressing.
func TestBindingCacheEvictsLeastRecentlyUsed(t *testing.T) {
	var opens, compiles atomic.Int64
	assembly := bindingAssembly(t, &opens, &compiles, nil)
	cache := NewBindingCache(assembly)
	profileRef := func(index int) ModelRef {
		pinned := bindingRef
		pinned.Profile = fmt.Sprintf("p%d", index)
		return pinned
	}

	// Fill the cache to capacity, oldest first: p1 is the least recently used.
	for index := 1; index <= maxCachedBindings; index++ {
		if _, err := cache.Bind(context.Background(), profileRef(index)); err != nil {
			t.Fatalf("Bind p%d: %v", index, err)
		}
	}
	if got := len(cache.cached); got != maxCachedBindings {
		t.Fatalf("cache holds %d bindings, want %d", got, maxCachedBindings)
	}
	// Using p1 makes p2 the least recently used entry.
	if _, err := cache.Bind(context.Background(), profileRef(1)); err != nil {
		t.Fatalf("Bind p1 again: %v", err)
	}
	if _, err := cache.Bind(context.Background(), profileRef(maxCachedBindings+1)); err != nil {
		t.Fatalf("Bind a new profile: %v", err)
	}
	if got := len(cache.cached); got != maxCachedBindings {
		t.Fatalf("cache holds %d bindings after eviction, want %d", got, maxCachedBindings)
	}
	if _, ok := cache.cached[profileRef(1)]; !ok {
		t.Error("the recently used binding was evicted")
	}
	if _, ok := cache.cached[profileRef(2)]; ok {
		t.Error("the least recently used binding survived")
	}
	if _, ok := cache.cached[profileRef(maxCachedBindings+1)]; !ok {
		t.Error("the newly opened binding was not retained")
	}
	// Everything still cached is served without reopening.
	before := opens.Load()
	for index := 1; index <= maxCachedBindings; index++ {
		if index == 2 {
			continue // evicted: legitimately reopens
		}
		if _, err := cache.Bind(context.Background(), profileRef(index)); err != nil {
			t.Fatalf("Bind p%d: %v", index, err)
		}
	}
	if got := opens.Load(); got != before {
		t.Fatalf("retained bindings reopened %d times", got-before)
	}
}

// TestBindingCacheSharesOneOpenAcrossConcurrentCallers pins the in-flight
// sharing: a burst of first calls opens the model once.
func TestBindingCacheSharesOneOpenAcrossConcurrentCallers(t *testing.T) {
	var opens, compiles atomic.Int64
	assembly := bindingAssembly(t, &opens, &compiles, nil)
	cache := NewBindingCache(assembly)

	const workers = 16
	var wait sync.WaitGroup
	bindings := make([]*Binding, workers)
	errs := make([]error, workers)
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(slot int) {
			defer wait.Done()
			bindings[slot], errs[slot] = cache.Bind(context.Background(), bindingRef)
		}(index)
	}
	wait.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("Bind %d: %v", index, err)
		}
		if bindings[index] != bindings[0] {
			t.Fatalf("Bind %d returned a different binding", index)
		}
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("opens = %d, want 1", got)
	}
}

// TestBindingCacheDoesNotCacheFailures pins that a credential configured after
// a failed open is picked up by the next call.
func TestBindingCacheDoesNotCacheFailures(t *testing.T) {
	var opens, compiles atomic.Int64
	assembly := bindingAssembly(t, &opens, &compiles,
		errors.New("profile needs an api_key"))
	cache := NewBindingCache(assembly)

	if _, err := cache.Bind(context.Background(), bindingRef); err == nil {
		t.Fatal("the first Bind must surface the missing credential")
	}
	if len(cache.cached) != 0 {
		t.Fatalf("cache retained %d bindings after a failure", len(cache.cached))
	}
	if _, err := cache.Bind(context.Background(), bindingRef); err == nil {
		t.Fatal("the failure must be retried, not cached as success")
	}
	if got := opens.Load(); got != 2 {
		t.Fatalf("opens = %d, want 2 (each Bind retried the open)", got)
	}
}

// TestBindingCachePrepareHelpersReuseTheBinding pins the convenience path used
// by hosts: preparing twice shares one open and compiles per request.
func TestBindingCachePrepareHelpersReuseTheBinding(t *testing.T) {
	var opens, compiles atomic.Int64
	assembly := bindingAssembly(t, &opens, &compiles, nil)
	cache := NewBindingCache(assembly)
	ctx := context.Background()

	first, err := cache.PrepareGenerate(ctx, bindingRef, bindingRequest())
	if err != nil {
		t.Fatalf("PrepareGenerate: %v", err)
	}
	second, err := cache.PrepareGenerate(ctx, bindingRef, bindingRequest())
	if err != nil {
		t.Fatalf("PrepareGenerate: %v", err)
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("opens after two prepares = %d, want 1", got)
	}
	if got := compiles.Load(); got != 2 {
		t.Fatalf("compiles after two prepares = %d, want 2", got)
	}
	for name, prepared := range map[string]*Prepared[GenerateResponse]{
		"first": first, "second": second,
	} {
		if _, err := prepared.Execute(ctx); err != nil {
			t.Fatalf("%s Execute: %v", name, err)
		}
	}
	if got := opens.Load(); got != 1 {
		t.Fatalf("opens after execution = %d, want 1", got)
	}
}

// TestBindingCacheWithoutAssembly pins the defensive path: a cache built with
// no assembly reports it instead of panicking.
func TestBindingCacheWithoutAssembly(t *testing.T) {
	cache := NewBindingCache(nil)
	if _, err := cache.Bind(context.Background(), bindingRef); err == nil {
		t.Fatal("Bind without an assembly must fail")
	}
	if _, err := (*BindingCache)(nil).Bind(context.Background(), bindingRef); err == nil {
		t.Fatal("Bind on a nil cache must fail")
	}
}
