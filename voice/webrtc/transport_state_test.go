package webrtc_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/pion/webrtc/v4"
)

// readErrAsync reads one value from the stream in a goroutine and reports the
// resulting error. The goroutine is unblocked at test cleanup (tr.Close
// interrupts the source pipe) when the stream is never interrupted by the state
// change under test.
func readErrAsync(s audio.Stream[audio.Frame]) <-chan error {
	ch := make(chan error, 1)
	go func() {
		_, err := s.Read()
		ch <- err
	}()
	return ch
}

// TestTransport_ConnectionStateDisconnected_DoesNotInterruptSource locks in the
// issue #2 fix: PeerConnectionStateDisconnected is a transient ICE state that
// routinely flaps and can recover, so it must NOT interrupt the source pipe (an
// interrupt would latch context.Canceled and kill a recoverable session).
func TestTransport_ConnectionStateDisconnected_DoesNotInterruptSource(t *testing.T) {
	tr := newTransport(t)
	if err := tr.InitPCForTest(); err != nil {
		t.Fatalf("InitPCForTest: %v", err)
	}

	tr.SimulateConnectionState(t, webrtc.PeerConnectionStateDisconnected)

	errCh := readErrAsync(tr.Source().Stream())
	select {
	case err := <-errCh:
		t.Fatalf("Disconnected interrupted the source pipe (Read returned %v); "+
			"it is transient and must not interrupt", err)
	case <-time.After(300 * time.Millisecond):
		// Still blocking with no data => not interrupted, as required.
		// t.Cleanup(tr.Close) unblocks the pending Read.
	}
}

// TestTransport_ConnectionStateFailed_InterruptsSourcePipe locks in that the
// terminal Failed state DOES interrupt the source pipe.
func TestTransport_ConnectionStateFailed_InterruptsSourcePipe(t *testing.T) {
	tr := newTransport(t)
	if err := tr.InitPCForTest(); err != nil {
		t.Fatalf("InitPCForTest: %v", err)
	}

	tr.SimulateConnectionState(t, webrtc.PeerConnectionStateFailed)

	errCh := readErrAsync(tr.Source().Stream())
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Failed: Read err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Failed did not interrupt the source pipe")
	}
}

// TestTransport_ConnectionStateClosed_InterruptsSourcePipe locks in that the
// terminal Closed state DOES interrupt the source pipe.
func TestTransport_ConnectionStateClosed_InterruptsSourcePipe(t *testing.T) {
	tr := newTransport(t)
	if err := tr.InitPCForTest(); err != nil {
		t.Fatalf("InitPCForTest: %v", err)
	}

	tr.SimulateConnectionState(t, webrtc.PeerConnectionStateClosed)

	errCh := readErrAsync(tr.Source().Stream())
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Closed: Read err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Closed did not interrupt the source pipe")
	}
}
