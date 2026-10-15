package plugin

import (
	"context"
	"reflect"
	"sort"

	"github.com/GizClaw/flowcraft/core/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

// watchRevisions starts the goroutine that keeps the running plugins in
// step with what the store says about them.
//
// The store's revision is the one stream every change goes through — an
// install, an update, a rollback, an uninstall, an enable, whatever
// wrote the enable state — so a host that watches it cannot be left
// serving a plugin version the record no longer names. The host's own
// operations go through the same stream and are already in step by the
// time the pass runs, which is what makes the pass idempotent.
func (h *Host) watchRevisions() {
	ctx, cancel := context.WithCancel(context.Background())
	signal := make(chan struct{}, 1)
	unsubscribe := h.store.Subscribe(func() {
		select {
		case signal <- struct{}{}:
		default:
		}
	})
	done := make(chan struct{})
	h.mu.Lock()
	h.watchCancel = cancel
	h.watchUnsubscribe = unsubscribe
	h.watchDone = done
	h.mu.Unlock()
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-signal:
				h.reconcile(ctx)
			}
		}
	}()
}

// stopRevisionWatch ends the watch and waits for the pass in flight, so
// Close cannot race a reconciliation that would start a process behind
// it.
func (h *Host) stopRevisionWatch() {
	h.mu.Lock()
	cancel := h.watchCancel
	unsubscribe := h.watchUnsubscribe
	done := h.watchDone
	h.watchCancel, h.watchUnsubscribe, h.watchDone = nil, nil, nil
	h.mu.Unlock()
	if unsubscribe != nil {
		unsubscribe()
	}
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// reconcile brings every running plugin into line with what the store
// says about it, and reports what it had to change.
//
// Only the running side is reconciled: a record that says enabled about
// a plugin whose process gave up is a state the application was told
// about and is the application's to change (see [Host.OnStopped]), so
// the pass never starts a process on its own.
func (h *Host) reconcile(ctx context.Context) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	ids := make([]string, 0, len(h.live))
	for id := range h.live {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	sort.Strings(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		h.reconcileEntry(ctx, id)
	}
}

// reconcileEntry is one plugin's share of a reconciliation pass. The
// entry lock is what makes the pass safe to run beside an operation of
// the application's own: a change in flight holds it, so the pass sees
// the state that change committed rather than the one in the middle of
// it.
func (h *Host) reconcileEntry(ctx context.Context, id string) {
	lock := h.entryLock(id)
	lock.Lock()
	defer lock.Unlock()
	h.mu.Lock()
	live, running := h.live[id]
	closed := h.closed
	h.mu.Unlock()
	if !running || closed {
		return
	}
	entry, ok := h.store.Entry(id)
	if !ok || entry.Error != "" || entry.Conflict != "" || !entry.Enabled {
		// The record does not ask for this plugin any more: someone took
		// it away (or disabled it, or replaced it with something that
		// cannot be published) while its process kept running.
		telemetry.Warn(ctx,
			"plugin host: a running plugin is no longer the one the record asks for; stopping it",
			attribute.String("plugin", id))
		if err := h.stopEntryLocked(ctx, id, StopRequested); err != nil {
			_ = h.abandonLocked(ctx, id, err)
		}
		return
	}
	if sameProcess(live.entry, entry) {
		return
	}
	// The record names a different version, different grants or another
	// directory than the process was started with. The record is what
	// the application reads, so the process follows it.
	telemetry.Warn(ctx,
		"plugin host: a running plugin is not the version the record names; restarting it",
		attribute.String("plugin", id))
	if err := h.stopEntryLocked(ctx, id, StopRequested); err != nil {
		// The process was released but a call did not finish in time;
		// the record must not keep saying enabled about it.
		_ = h.abandonLocked(ctx, id, err)
		return
	}
	if err := h.startAgain(ctx, id, true); err != nil {
		telemetry.WarnErr(ctx,
			"plugin host: the plugin the record names did not come up", err,
			attribute.String("plugin", id))
		_ = h.abandonLocked(ctx, id, err)
	}
}

// sameProcess reports whether a running plugin's process was started
// from what the store says about it now. The manifest is compared by
// value — it is parsed again for every read, so the pointers are not the
// same and the contents are what matters — and the directory with it,
// because a user copy that starts shadowing a builtin is a different
// plugin on disk under the same id.
func sameProcess(live, current Entry) bool {
	return live.Dir == current.Dir &&
		reflect.DeepEqual(live.Manifest, current.Manifest)
}
