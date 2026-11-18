package bytedance

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/outcome"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// sniffImageMediaType is the only thing standing between a base64 answer and a
// part the model can read: the platform does not send a media type with the
// bytes, so the header is the media type.
func TestSniffImageMediaType(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 8)...), "image/png"},
		{"jpeg", append([]byte("\xff\xd8\xff\xe0"), make([]byte, 8)...), "image/jpeg"},
		{"webp", append([]byte("RIFF\x00\x00\x00\x00WEBP"), make([]byte, 4)...), "image/webp"},
		{"gif is not served", append([]byte("GIF89a"), make([]byte, 8)...), ""},
		{"text", []byte("not an image at all"), ""},
		{"empty", nil, ""},
		// A RIFF container that is not WEBP — a wav, say — must not be read as
		// an image because the first four bytes matched.
		{"riff but not webp", append([]byte("RIFF\x00\x00\x00\x00WAVE"), make([]byte, 4)...), ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := sniffImageMediaType(test.data); got != test.want {
				t.Fatalf("sniff = %q, want %q", got, test.want)
			}
		})
	}
}

// imagePartFromB64 is the inline delivery: the bytes arrive base64-encoded and
// carry no media type, so the part is built from the sniffed header.
func TestImagePartFromB64(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	part, err := imagePartFromB64(base64.StdEncoding.EncodeToString(png))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if part.Source.MediaType() != "image/png" {
		t.Errorf("media type = %q, want image/png", part.Source.MediaType())
	}
	if part.Source.Kind() != media.SourceInline || len(part.Source.Bytes()) != len(png) {
		t.Errorf("source = %s with %d bytes, want inline with %d",
			part.Source.Kind(), len(part.Source.Bytes()), len(png))
	}

	// A payload that is not base64 is a decode failure, and one that decodes to
	// something without an image header is a media-type failure: both have to
	// stop the turn rather than hand the model bytes it cannot read.
	if _, err := imagePartFromB64("not base64!"); err == nil {
		t.Error("an unencoded payload was accepted")
	}
	if _, err := imagePartFromB64(base64.StdEncoding.EncodeToString([]byte("plain text"))); err == nil {
		t.Error("a payload with no image header was accepted")
	}
}

// sourceURI is where inline bytes become a data URI for the platform's reference
// fields. It is the only conversion of its kind in the package, and it decides
// between a URL and an encoding.
func TestSourceURI(t *testing.T) {
	inline, err := media.NewImageBytes([]byte("png"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	if got := sourceURI(inline); !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Errorf("inline source = %q, want a data URI", got)
	}
	link, err := media.NewImageURL("https://example.test/frame.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	if got := sourceURI(link); got != "https://example.test/frame.png" {
		t.Errorf("url source = %q, want the URL itself", got)
	}
}

// decodeImage turns the wire answer into parts: a URL answer and an inline one
// take different paths, and the usage counts what was generated.
func TestDecodeImageParts(t *testing.T) {
	raw := imageRaw{
		mediaType:    "image/png",
		requestID:    "req-1",
		inputTokens:  12,
		outputTokens: 34,
		images: []rawImage{
			{url: "https://example.test/a.png"},
			{b64: base64.StdEncoding.EncodeToString(
				append([]byte("\xff\xd8\xff\xe0"), make([]byte, 8)...))},
		},
	}
	response, err := decodeImage(context.Background(), testRequest("image-test", inference.OutputImage, message.NewTextContent("x")), raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(response.Message.Content.Parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(response.Message.Content.Parts))
	}
	if got := response.Usage.WorkValue(outcome.WorkImagesGenerated); got != 2 {
		t.Errorf("images generated = %d, want 2", got)
	}
	if response.Metadata.RequestID != "req-1" {
		t.Errorf("request id = %q", response.Metadata.RequestID)
	}
	if response.Usage.InputTokens != 12 || response.Usage.OutputTokens != 34 {
		t.Errorf("usage = %+v", response.Usage)
	}

	// An image carrying neither form is the platform contradicting itself, and
	// the turn fails rather than handing back an empty part.
	if _, err := decodeImage(context.Background(), testRequest("image-test", inference.OutputImage, message.NewTextContent("x")),
		imageRaw{images: []rawImage{{}}}); err == nil {
		t.Error("an image with neither url nor data was accepted")
	}
}
