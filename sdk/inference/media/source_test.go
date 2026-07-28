package media

import (
	"encoding/json"
	"testing"
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
