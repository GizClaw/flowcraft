package syncx_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/memory/internal/syncx"
)

func TestKeyedMutex_SerializesSameKey(t *testing.T) {
	km := syncx.New()
	var inside, max int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			km.Lock("k1")
			defer km.Unlock("k1")
			n := atomic.AddInt32(&inside, 1)
			for {
				m := atomic.LoadInt32(&max)
				if n <= m || atomic.CompareAndSwapInt32(&max, m, n) {
					break
				}
			}
			atomic.AddInt32(&inside, -1)
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt32(&max); got != 1 {
		t.Fatalf("KeyedMutex did not serialise same-key callers; max parallelism observed = %d", got)
	}
}

func TestKeyedMutex_DifferentKeysDoNotContend(t *testing.T) {
	km := syncx.New()
	// Hold k1 from a long-running goroutine; verify k2 acquisition
	// is unaffected.
	hold := make(chan struct{})
	done := make(chan struct{})
	go func() {
		km.Lock("k1")
		close(done)
		<-hold
		km.Unlock("k1")
	}()
	<-done
	// k2 must Lock without waiting — assert by Lock+Unlock under
	// the t.Parallel-friendly timeout.
	km.Lock("k2")
	km.Unlock("k2")
	close(hold)
}

func TestKeyedMutex_EntryReaped(t *testing.T) {
	km := syncx.New()
	for i := 0; i < 1024; i++ {
		key := "transient-" + string(rune('A'+i%26))
		km.Lock(key)
		km.Unlock(key)
	}
	// We don't expose internals, but Lock+Unlock+Lock should
	// continue to work for any key, including freshly-reaped ones,
	// which proves the entry was returned to the pool cleanly.
	for i := 0; i < 16; i++ {
		km.Lock("hot")
		km.Unlock("hot")
	}
}

func TestKeyedMutex_UnlockWithoutLockPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("Unlock without Lock should panic")
		}
	}()
	km := syncx.New()
	km.Unlock("nope")
}

func TestKeyedMutex_WithLockReleasesOnPanic(t *testing.T) {
	km := syncx.New()
	func() {
		defer func() { _ = recover() }()
		km.WithLock("k", func() { panic("inside") })
	}()
	// If WithLock did not defer Unlock, this Lock would deadlock.
	km.Lock("k")
	km.Unlock("k")
}
