package plugin

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestUninstallHoldsThePluginsMigrationLock pins the order an uninstall
// runs in: everything before the store change — the checks and the stop
// — and the store change itself are one migration of this plugin, held
// under its entry lock, so an enable that races the uninstall waits for
// it instead of starting a process from the directory being removed.
//
// The observation point is the store's own revision callback: it runs
// inside Store.Uninstall, with the removal done. A callback that finds
// the entry lock free is a callback that ran while another migration
// could have interleaved.
func TestUninstallHoldsThePluginsMigrationLock(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writePlugin(t, root, "hello", manifestFor("hello", "0.1.0"))
	store := newStoreOver(t, root)
	host := startHost(t, store, newVersionFactory().new)

	lock := host.entryLock("hello")
	var (
		mu      sync.Mutex
		held    []bool
		callbck = make(chan struct{}, 4)
	)
	unsubscribe := store.Subscribe(func() {
		mu.Lock()
		held = append(held, !lock.TryLock())
		mu.Unlock()
		select {
		case callbck <- struct{}{}:
		default:
		}
	})
	defer unsubscribe()

	if err := host.Uninstall(context.Background(), "hello", UninstallOptions{}); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	select {
	case <-callbck:
	case <-time.After(5 * time.Second):
		t.Fatal("the store never reported the uninstall")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(held) == 0 {
		t.Fatal("no revision callback ran")
	}
	for _, locked := range held {
		if !locked {
			t.Fatal("the store changed with the plugin's entry lock free")
		}
	}
	if _, ok := store.Entry("hello"); ok {
		t.Fatal("the plugin is still installed")
	}
}
