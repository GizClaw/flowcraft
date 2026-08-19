package media

import (
	"context"
	"encoding/json"
	"strings"
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

func TestStreamSourcesCarryLiveStreams(t *testing.T) {
	pipe := NewPipe[string](1)
	if !pipe.Send("live") {
		t.Fatal("pipe rejected its first item")
	}
	audio, err := NewAudioStream[string](pipe, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioStream: %v", err)
	}
	if audio.Kind() != SourceStream {
		t.Fatalf("audio kind = %q, want %q", audio.Kind(), SourceStream)
	}
	if audio.MediaType() != "audio/pcm" {
		t.Fatalf("audio media type = %q", audio.MediaType())
	}
	stream, ok := audio.Stream().(Stream[string])
	if !ok {
		t.Fatalf("audio Stream() = %T, want Stream[string]", audio.Stream())
	}
	value, err := stream.Read(context.Background())
	if err != nil || value != "live" {
		t.Fatalf("stream read = (%q, %v)", value, err)
	}

	video, err := NewVideoStream[string](pipe, "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoStream: %v", err)
	}
	if video.Kind() != SourceStream || video.MediaType() != "video/mp4" {
		t.Fatalf("video stream source = kind %q type %q", video.Kind(), video.MediaType())
	}
}

func TestStreamSourcesRejectBadInput(t *testing.T) {
	if _, err := NewAudioStream[string](nil, "audio/pcm"); err == nil {
		t.Fatal("audio stream accepted a nil stream")
	}
	var nilPipe *Pipe[string]
	if _, err := NewAudioStream[string](nilPipe, "audio/pcm"); err == nil {
		t.Fatal("audio stream accepted a typed nil stream")
	}
	pipe := NewPipe[string](0)
	if _, err := NewAudioStream[string](pipe, ""); err == nil {
		t.Fatal("audio stream accepted an empty media type")
	}
	if _, err := NewAudioStream[string](pipe, "video/mp4"); err == nil {
		t.Fatal("audio stream accepted a video media type")
	}
	if _, err := NewVideoStream[string](pipe, "image/jpeg"); err == nil {
		t.Fatal("video stream accepted an image media type")
	}
}

func TestStreamSourcesAreNotSerializable(t *testing.T) {
	pipe := NewPipe[string](0)
	audio, err := NewAudioStream[string](pipe, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioStream: %v", err)
	}
	if _, err := json.Marshal(audio); err == nil ||
		!strings.Contains(err.Error(), "cannot be serialized") {
		t.Fatalf("Marshal(stream source) = %v, want serialization error", err)
	}
	if err := json.Unmarshal([]byte(`{"kind":"stream","media_type":"audio/pcm"}`), &audio); err == nil {
		t.Fatal("Unmarshal accepted a stream-kind wire source without a stream")
	}
}

func TestStreamSourceCloneSharesLiveHandle(t *testing.T) {
	pipe := NewPipe[string](0)
	audio, err := NewAudioStream[string](pipe, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioStream: %v", err)
	}
	clone := audio.Clone()
	if clone.Kind() != SourceStream {
		t.Fatalf("clone kind = %q", clone.Kind())
	}
	if clone.Stream() != audio.Stream() {
		t.Fatal("clone does not share the live stream handle")
	}
}
