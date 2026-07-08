package webrtc

import (
	"reflect"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// NewSinkForTest creates a Sink with the given track and encoder.
// Exported only for testing via the _test build tag.
func NewSinkForTest(track interface{ WriteSample(media.Sample) error }, encoder AudioEncoder) *Sink {
	return newSink(track, encoder)
}

// InitPCForTest runs the real initPC so the production OnConnectionStateChange
// handler is registered on a live PeerConnection, without performing an SDP
// handshake. Exported only for testing.
func (t *Transport) InitPCForTest() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.initPC(false)
}

// SimulateConnectionState invokes the REAL OnConnectionStateChange handler that
// initPC registered on the underlying pion PeerConnection, passing the given
// state. This lets tests exercise the production state -> source-interrupt
// decision (only Failed/Closed interrupt; Disconnected is transient) directly,
// without provoking a real ICE failure.
//
// The handler is an unexported closure stored in an unexported atomic.Value
// field of pion's PeerConnection with no public trigger, so it is read via
// reflection. This is deterministic (not flaky) but pinned to the pion version
// in go.mod; if pion renames that field this shim breaks.
//
// If the reflection shim cannot resolve the handler (e.g. a pion upgrade
// renamed/moved the field), it returns nil. Silently skipping the call would
// make every connection-state test pass VACUOUSLY — in particular the
// Disconnected test, whose success condition is merely "the source pipe was NOT
// interrupted", would then pass even though the production handler never ran.
// To make a broken shim fail loudly instead of masquerading as a green test,
// this fatals when the handler is nil.
func (t *Transport) SimulateConnectionState(tb testing.TB, state webrtc.PeerConnectionState) {
	tb.Helper()
	h := connectionStateHandler(t.pc)
	if h == nil {
		tb.Fatalf("SimulateConnectionState: connectionStateHandler returned nil — the pion " +
			"reflection shim is broken (field renamed/moved after a pion upgrade?). " +
			"Connection-state tests cannot run and must not pass vacuously.")
	}
	h(state)
}

func connectionStateHandler(pc *webrtc.PeerConnection) func(webrtc.PeerConnectionState) {
	if pc == nil {
		return nil
	}
	f := reflect.ValueOf(pc).Elem().FieldByName("onConnectionStateChangeHandler")
	if !f.IsValid() || !f.CanAddr() {
		return nil
	}
	av := (*atomic.Value)(unsafe.Pointer(f.UnsafeAddr()))
	h, _ := av.Load().(func(webrtc.PeerConnectionState))
	return h
}

// NotifyTrackReady exposes the trackReady signal for concurrency testing.
func (t *Transport) NotifyTrackReady() {
	t.trackReadyOnce.Do(func() { close(t.trackReady) })
}

// TrackReady returns the channel that is closed when a remote audio track arrives.
func (t *Transport) TrackReady() <-chan struct{} {
	return t.trackReady
}
