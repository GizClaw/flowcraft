package media

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/goccy/go-json"
)

func TestTypedSourcesRoundTripAndOwnInlineBytes(t *testing.T) {
	raw := []byte("audio")
	audio, err := NewAudioBytes(raw, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	raw[0] = 'X'
	if string(audio.Bytes()) != "audio" {
		t.Fatal("audio source retained caller byte storage")
	}
	data, err := json.Marshal(audio)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded AudioSource
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.Kind() != SourceInline || string(decoded.Bytes()) != "audio" {
		t.Fatalf("decoded source = %+v", decoded)
	}

	image, err := NewImageURL("https://example.com/image.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	video, err := NewVideoURL("https://example.com/video.mp4", "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoURL: %v", err)
	}
	if image.URL() == "" || video.URL() == "" {
		t.Fatal("typed URL sources lost their URL")
	}
}

func TestTypedSourcesRejectMismatchedMediaTypes(t *testing.T) {
	if _, err := NewImageBytes([]byte("audio"), "audio/mpeg"); err == nil {
		t.Fatal("image source accepted audio media type")
	}
	if _, err := NewAudioURL("https://example.com/audio", "video/mp4"); err == nil {
		t.Fatal("audio source accepted video media type")
	}
	if _, err := NewVideoBytes([]byte("video"), "image/jpeg"); err == nil {
		t.Fatal("video source accepted image media type")
	}
	image, err := NewImageURL(
		"https://example.com/image",
		"IMAGE/PNG; name=result.png",
	)
	if err != nil {
		t.Fatalf("normalized image URL: %v", err)
	}
	if image.MediaType() != "image/png; name=result.png" {
		t.Fatalf("normalized media type = %q", image.MediaType())
	}
	audio, err := NewAudioURL(
		"https://example.com/audio",
		"audio/webm; codecs=opus",
	)
	if err != nil {
		t.Fatalf("parameterized audio URL: %v", err)
	}
	if audio.MediaType() != "audio/webm; codecs=opus" ||
		audio.BaseMediaType() != "audio/webm" {
		t.Fatalf("audio media type lost parameters: %q", audio.MediaType())
	}
}

func TestInlineImageCopiesInput(t *testing.T) {
	raw := []byte("secret image")
	source, err := NewImageBytes(raw, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	raw[0] = 'X'
	if got := string(source.Bytes()); got != "secret image" {
		t.Fatalf("source bytes mutated through caller slice: %q", got)
	}
	copyOut := source.Bytes()
	copyOut[0] = 'X'
	if got := string(source.Bytes()); got != "secret image" {
		t.Fatalf("source bytes mutated through returned slice: %q", got)
	}
}

// TestDataURLIsExact pins the encoding to the concatenation it replaced. A
// byte-level drift here changes what a provider receives, so the comparison is
// against the reference spelling rather than against a golden the helper itself
// produced.
func TestDataURLIsExact(t *testing.T) {
	for _, mediaType := range []string{"image/png", "text/plain; charset=utf-8", ""} {
		for _, size := range []int{0, 1, 2, 3, 4, 255, 256, 4096} {
			data := make([]byte, size)
			for i := range data {
				data[i] = byte(i * 7)
			}
			want := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
			if got := DataURL(mediaType, data); got != want {
				t.Fatalf("type %q size %d: DataURL drifted from the reference spelling", mediaType, size)
			}
		}
	}
}

func TestBase64IsExact(t *testing.T) {
	for _, size := range []int{0, 1, 2, 3, 4, 4096} {
		data := make([]byte, size)
		for i := range data {
			data[i] = byte(i)
		}
		if got, want := Base64(data), base64.StdEncoding.EncodeToString(data); got != want {
			t.Fatalf("size %d: Base64 drifted from the reference spelling", size)
		}
	}
}

// TestDataURLAllocations guards the reason the helper exists: one buffer for the
// header and the payload, then one string to hand out. The old spelling allocated
// the encoded payload, the concatenated result, and the intermediate prefix.
func TestDataURLAllocations(t *testing.T) {
	data := make([]byte, 64<<10)
	// Two allocations: the buffer and the string handed out. The concatenation
	// this replaced needed three, so a cap of three would let a revert pass.
	if allocs := testing.AllocsPerRun(100, func() { _ = DataURL("image/png", data) }); allocs > 2 {
		t.Fatalf("DataURL allocates %.0f times, want <= 2", allocs)
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = Base64(data) }); allocs > 2 {
		t.Fatalf("Base64 allocates %.0f times, want <= 2", allocs)
	}
}

func BenchmarkDataURL(b *testing.B) {
	for _, size := range []struct {
		name string
		len  int
	}{
		{"4KiB", 4 << 10},
		{"1MiB", 1 << 20},
	} {
		data := make([]byte, size.len)
		b.Run(size.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size.len))
			for i := 0; i < b.N; i++ {
				if !strings.HasPrefix(DataURL("image/png", data), dataURLScheme) {
					b.Fatal("missing scheme")
				}
			}
		})
	}
}
