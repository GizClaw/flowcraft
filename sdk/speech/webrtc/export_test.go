package webrtc

import "github.com/pion/webrtc/v4/pkg/media"

// NewSinkForTest creates a Sink with the given track and encoder.
// Exported only for testing via the _test build tag.
func NewSinkForTest(track interface{ WriteSample(media.Sample) error }, encoder AudioEncoder) *Sink {
	return newSink(track, encoder)
}

// NotifyTrackReady exposes the trackReady signal for concurrency testing.
func (t *Transport) NotifyTrackReady() {
	t.trackReadyOnce.Do(func() { close(t.trackReady) })
}

// TrackReady returns the channel that is closed when a remote audio track arrives.
func (t *Transport) TrackReady() <-chan struct{} {
	return t.trackReady
}
