package background

import (
	"testing"
	"time"
)

func TestDebouncerCoalescesResets(t *testing.T) {
	d := NewDebouncer(50 * time.Millisecond)
	defer d.Stop()

	if !d.Reset() {
		t.Fatal("first Reset returned false")
	}
	time.Sleep(10 * time.Millisecond)
	if !d.Reset() {
		t.Fatal("second Reset returned false")
	}

	select {
	case <-d.C():
		t.Fatal("debouncer fired before quiet period elapsed")
	case <-time.After(20 * time.Millisecond):
	}

	select {
	case <-d.C():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("debouncer did not fire after quiet period")
	}

	select {
	case <-d.C():
		t.Fatal("debouncer emitted more than one signal for coalesced resets")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestDebouncerResetDrainsPendingSignal(t *testing.T) {
	d := NewDebouncer(40 * time.Millisecond)
	defer d.Stop()

	if !d.Reset() {
		t.Fatal("first Reset returned false")
	}
	waitForPendingSignal(t, d)

	if !d.Reset() {
		t.Fatal("second Reset returned false")
	}
	select {
	case <-d.C():
		t.Fatal("Reset did not drain stale pending signal")
	case <-time.After(10 * time.Millisecond):
	}

	select {
	case <-d.C():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("debouncer did not fire after reset")
	}
}

func TestDebouncerStopPreventsSignals(t *testing.T) {
	d := NewDebouncer(10 * time.Millisecond)
	if !d.Reset() {
		t.Fatal("Reset returned false")
	}
	d.Stop()
	if d.Reset() {
		t.Fatal("Reset succeeded after Stop")
	}

	select {
	case <-d.C():
		t.Fatal("debouncer emitted after Stop")
	case <-time.After(30 * time.Millisecond):
	}
}

func waitForPendingSignal(t *testing.T, d *Debouncer) {
	t.Helper()
	deadline := time.After(200 * time.Millisecond)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for pending signal")
		case <-tick.C:
			d.mu.Lock()
			n := len(d.c)
			d.mu.Unlock()
			if n > 0 {
				return
			}
		}
	}
}
