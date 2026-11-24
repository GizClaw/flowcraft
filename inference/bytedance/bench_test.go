package bytedance

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/model"
	"github.com/GizClaw/flowcraft/inference/pipeline"
)

// benchImagePart is one inline reference, the shape the media surfaces have to
// turn into a data URI before the request can go out.
func benchImagePart(tb testing.TB, size int) message.Part {
	tb.Helper()
	source, err := media.NewImageBytes(make([]byte, size), "image/png")
	if err != nil {
		tb.Fatalf("NewImageBytes: %v", err)
	}
	return message.ImagePart{Source: source}
}

// BenchmarkCompileImage is the image request build: prompt assembly, option
// mapping, and the sequential/optimize guards. No media encoding happens here,
// so the number is the per-call overhead every image request pays.
func BenchmarkCompileImage(b *testing.B) {
	desc := testDescriptor("image-bench", "image",
		[]message.PartKind{message.PartText}, []message.PartKind{message.PartImage})
	request := testRequest("image-bench", inference.OutputImage,
		message.NewTextContent("a red square on a white wall, photographed in daylight"))
	compile := compileImage("ep-image", desc)
	b.ReportAllocs()
	b.SetBytes(int64(len("a red square on a white wall, photographed in daylight")))
	for i := 0; i < b.N; i++ {
		if _, err := compile(context.Background(), request, pipeline.ShapeUnary); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCompileVideo is the video request build with a reference image: the
// part conversion plus the data-URI encoding of the bytes, which is the only
// place this driver copies media.
func BenchmarkCompileVideo(b *testing.B) {
	spec := ModelSpec{
		Name: "video-bench", Kind: "video",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText, message.PartImage},
			Outputs: []message.PartKind{message.PartVideo},
		},
	}
	request := testRequest("video-bench", inference.OutputVideo, message.Content{
		Parts: []message.Part{
			message.TextPart{Text: "a slow dolly shot through a neon-lit alley"},
			benchImagePart(b, 512<<10),
		},
	})
	compile := compileVideo("ep-video", spec)
	b.ReportAllocs()
	b.SetBytes(512 << 10)
	for i := 0; i < b.N; i++ {
		if _, err := compile(context.Background(), request, pipeline.ShapeUnary); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCompileEmbed is the text batch build: one item per input, which is
// what a retrieval pipeline sends on every query.
func BenchmarkCompileEmbed(b *testing.B) {
	desc := testDescriptor("embed-bench", "embed",
		[]message.PartKind{message.PartText}, nil)
	desc.Capabilities.EmbeddingDimensions = 2048
	inputs := make([]message.Content, 32)
	for i := range inputs {
		inputs[i] = message.NewTextContent("the quick brown fox jumps over the lazy dog")
	}
	request := inference.EmbedRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "embed-bench"}},
		Input: inputs,
	}
	compile := compileEmbed("ep-embed", desc)
	b.ReportAllocs()
	b.SetBytes(int64(32 * len("the quick brown fox jumps over the lazy dog")))
	for i := 0; i < b.N; i++ {
		if _, err := compile(context.Background(), request, pipeline.ShapeUnary); err != nil {
			b.Fatal(err)
		}
	}
}
