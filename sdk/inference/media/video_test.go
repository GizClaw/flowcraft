package media

import "testing"

func TestVideoFrameRequiresSourceAndNonNegativeTimestamp(t *testing.T) {
	source, err := NewImageBytes([]byte("frame"), "image/jpeg")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	if err := (VideoFrame{Source: source, TimestampMillis: 10}).Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := (VideoFrame{Source: source, TimestampMillis: -1}).Validate(); err == nil {
		t.Fatal("negative frame timestamp was accepted")
	}
}
