package message_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func TestPartPipeRoundTrip(t *testing.T) {
	pipe := message.NewPartPipe(2)
	if !pipe.Send(message.TextPart{Text: "hello"}) {
		t.Fatal("Send rejected")
	}
	item, err := pipe.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if item.Kind() != message.PartText {
		t.Fatalf("item kind = %q", item.Kind())
	}
}

func TestMaterializeAudioStreamIntoInlinePart(t *testing.T) {
	format := media.AudioFormat{
		Encoding:     media.AudioEncodingPCM16,
		SampleRateHz: 1000,
		Channels:     1,
	}
	chunk := func(data string) message.Part {
		source, err := media.NewAudioBytes([]byte(data), "audio/pcm")
		if err != nil {
			t.Fatalf("NewAudioBytes: %v", err)
		}
		return message.AudioPart{Source: source, Format: &format}
	}
	pipe := message.NewPartPipe(2)
	pipe.Send(chunk("abcd"))
	pipe.Send(chunk("ef"))
	pipe.Close()
	source, err := media.NewAudioStream(pipe, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioStream: %v", err)
	}
	content := message.Content{Parts: []message.Part{
		message.TextPart{Text: "transcript"},
		message.AudioPart{Source: source},
	}}

	materialized, err := message.MaterializeContent(context.Background(), content)
	if err != nil {
		t.Fatalf("MaterializeContent: %v", err)
	}
	if message.HasStreamSource(materialized) {
		t.Fatal("materialized content still carries a stream source")
	}
	if len(materialized.Parts) != 2 {
		t.Fatalf("materialized parts = %d, want 2", len(materialized.Parts))
	}
	audio, ok := materialized.Parts[1].(message.AudioPart)
	if !ok {
		t.Fatalf("materialized part 1 = %T, want AudioPart", materialized.Parts[1])
	}
	if audio.Source.Kind() != media.SourceInline {
		t.Fatalf("materialized source kind = %q", audio.Source.Kind())
	}
	if got := string(audio.Source.Bytes()); got != "abcdef" {
		t.Fatalf("materialized bytes = %q, want \"abcdef\"", got)
	}
	if audio.Format == nil || *audio.Format != format {
		t.Fatalf("materialized format = %v, want %v", audio.Format, format)
	}
	if audio.DurationMillis == nil || *audio.DurationMillis != 3 {
		t.Fatalf("materialized duration = %v, want 3ms", audio.DurationMillis)
	}
}

func TestMaterializeVideoStreamIntoInlinePart(t *testing.T) {
	chunk := func(data string) message.Part {
		source, err := media.NewVideoBytes([]byte(data), "video/mp4")
		if err != nil {
			t.Fatalf("NewVideoBytes: %v", err)
		}
		return message.VideoPart{Source: source}
	}
	pipe := message.NewPartPipe(2)
	pipe.Send(chunk("moov"))
	pipe.Send(chunk("mdat"))
	pipe.Close()
	source, err := media.NewVideoStream(pipe, "video/mp4")
	if err != nil {
		t.Fatalf("NewVideoStream: %v", err)
	}
	content := message.Content{Parts: []message.Part{
		message.VideoPart{Source: source},
	}}
	materialized, err := message.MaterializeContent(context.Background(), content)
	if err != nil {
		t.Fatalf("MaterializeContent: %v", err)
	}
	video, ok := materialized.Parts[0].(message.VideoPart)
	if !ok {
		t.Fatalf("materialized part = %T, want VideoPart", materialized.Parts[0])
	}
	if got := string(video.Source.Bytes()); got != "moovmdat" {
		t.Fatalf("materialized bytes = %q", got)
	}
}

func TestMaterializeContentWithoutStreamsIsUnchanged(t *testing.T) {
	source, err := media.NewAudioBytes([]byte("audio"), "audio/mpeg")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	content := message.Content{Parts: []message.Part{
		message.TextPart{Text: "hi"},
		message.AudioPart{Source: source},
	}}
	got, err := message.MaterializeContent(context.Background(), content)
	if err != nil {
		t.Fatalf("MaterializeContent: %v", err)
	}
	if !reflect.DeepEqual(got, content) {
		t.Fatal("materialization modified a stream-free content")
	}
}

func TestMaterializeStreamRejectsUnexpectedItems(t *testing.T) {
	pipe := message.NewPartPipe(1)
	pipe.Send(message.TextPart{Text: "oops"})
	pipe.Close()
	source, err := media.NewAudioStream(pipe, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioStream: %v", err)
	}
	content := message.Content{Parts: []message.Part{
		message.AudioPart{Source: source},
	}}
	if _, err := message.MaterializeContent(context.Background(), content); err == nil ||
		!strings.Contains(err.Error(), "want audio part") {
		t.Fatalf("MaterializeContent = %v, want unexpected-item error", err)
	}
}

func TestMaterializeEmptyStreamErrors(t *testing.T) {
	pipe := message.NewPartPipe(0)
	pipe.Close()
	source, err := media.NewAudioStream(pipe, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioStream: %v", err)
	}
	content := message.Content{Parts: []message.Part{
		message.AudioPart{Source: source},
	}}
	if _, err := message.MaterializeContent(context.Background(), content); err == nil ||
		!strings.Contains(err.Error(), "no audio data") {
		t.Fatalf("MaterializeContent = %v, want empty-stream error", err)
	}
}

func TestMaterializeInterruptedStreamErrors(t *testing.T) {
	pipe := message.NewPartPipe(0)
	pipe.Interrupt()
	source, err := media.NewAudioStream(pipe, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioStream: %v", err)
	}
	content := message.Content{Parts: []message.Part{
		message.AudioPart{Source: source},
	}}
	if _, err := message.MaterializeContent(context.Background(), content); err == nil {
		t.Fatal("MaterializeContent accepted an interrupted stream")
	}
}

func TestHasStreamSource(t *testing.T) {
	pipe := message.NewPartPipe(0)
	source, err := media.NewAudioStream(pipe, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioStream: %v", err)
	}
	inline, err := media.NewAudioBytes([]byte("x"), "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioBytes: %v", err)
	}
	if !message.HasStreamSource(message.Content{Parts: []message.Part{
		message.AudioPart{Source: source},
	}}) {
		t.Fatal("HasStreamSource missed a stream-backed audio part")
	}
	if message.HasStreamSource(message.Content{Parts: []message.Part{
		message.AudioPart{Source: inline},
		message.TextPart{Text: "x"},
	}}) {
		t.Fatal("HasStreamSource reported a stream source for inline parts")
	}
}

func TestStreamContentCannotBeJSONEncoded(t *testing.T) {
	pipe := message.NewPartPipe(0)
	source, err := media.NewAudioStream(pipe, "audio/pcm")
	if err != nil {
		t.Fatalf("NewAudioStream: %v", err)
	}
	content := message.Content{Parts: []message.Part{
		message.AudioPart{Source: source},
	}}
	if _, err := json.Marshal(content); err == nil {
		t.Fatal("Marshal accepted a stream-backed content")
	}
}
