package event

import (
	"context"
	"fmt"
	"testing"
)

// benchPublishHotSubject is the hot-subject scenario the route cache is
// designed for: many subscriptions, but a small set of subjects publishes
// against. Without the cache every Publish does an O(N) match scan; with
// it cache hits skip to O(matched).
//
// Run with:
//
//	go test -bench=BenchmarkPublish -benchmem ./event
func benchPublishHotSubject(b *testing.B, numSubs int, cacheSize int) {
	bus := NewMemoryBus(WithRouteCacheSize(cacheSize))
	defer func() { _ = bus.Close() }()
	ctx := context.Background()

	// Subscriptions all listen on disjoint patterns so that a publish to
	// "hot.subject" matches exactly zero of them — pure dispatch cost,
	// no channel traffic to perturb the measurement.
	for i := 0; i < numSubs; i++ {
		_, err := bus.Subscribe(ctx, Pattern(fmt.Sprintf("nomatch.p%d.>", i)))
		if err != nil {
			b.Fatalf("subscribe: %v", err)
		}
	}

	env := mustEnv(&testing.T{}, "hot.subject", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := bus.Publish(ctx, env); err != nil {
			b.Fatalf("publish: %v", err)
		}
	}
}

func BenchmarkPublish_HotSubject_100Subs_Cached(b *testing.B) {
	benchPublishHotSubject(b, 100, 1024)
}
func BenchmarkPublish_HotSubject_100Subs_NoCache(b *testing.B) {
	benchPublishHotSubject(b, 100, 0)
}
func BenchmarkPublish_HotSubject_1000Subs_Cached(b *testing.B) {
	benchPublishHotSubject(b, 1000, 1024)
}
func BenchmarkPublish_HotSubject_1000Subs_NoCache(b *testing.B) {
	benchPublishHotSubject(b, 1000, 0)
}
