package openai

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/message/media"
)

// BenchmarkDataURL is the guard for inline media rendering: every request that
// carries an image in its history re-encodes it, so this runs once per media
// part per turn.
//
// The sizes bracket the real range: a small avatar-ish payload and a
// screenshot-sized one, where the copy the current implementation makes twice
// shows up as a bandwidth gap.
func BenchmarkDataURL(b *testing.B) {
	for _, size := range []struct {
		name string
		len  int
	}{
		{"4KiB", 4 << 10},
		{"256KiB", 256 << 10},
		{"1MiB", 1 << 20},
	} {
		data := make([]byte, size.len)
		for i := range data {
			data[i] = byte(i)
		}
		b.Run(size.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size.len))
			for i := 0; i < b.N; i++ {
				if got := dataURL("image/png", data); got == "" {
					b.Fatal("empty data URL")
				}
			}
		})
	}
}

// BenchmarkAudioPayload is the other inline encoding on the chat surface: base64
// without a data-URL prefix, alongside the format token it is validated against.
func BenchmarkAudioPayload(b *testing.B) {
	data := make([]byte, 1<<20)
	source, err := media.NewAudioBytes(data, "audio/wav")
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		if _, _, err := audioPayload(source); err != nil {
			b.Fatal(err)
		}
	}
}
