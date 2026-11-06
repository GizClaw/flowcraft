package files

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// imageGroup builds the group with the image switch set the way a host wires it.
func imageGroup(t *testing.T, images bool) (*Tool, *fsx.Tree) {
	t.Helper()
	tree, err := fsx.Open(fsx.WithWritable(t.TempDir()))
	if err != nil {
		t.Fatalf("fsx.Open: %v", err)
	}
	t.Cleanup(func() { _ = tree.Close() })
	fileTools, err := New(tree, tree.Writable()[0], WithImages(images))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return fileTools, tree
}

// callContent runs a tool and keeps the content, because an image result has no
// text to return.
func callContent(t *testing.T, candidate tool.Tool, arguments map[string]any) (message.Content, error) {
	t.Helper()
	payload, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("marshal arguments: %v", err)
	}
	return candidate.Execute(context.Background(), string(payload))
}

func put(t *testing.T, tree *fsx.Tree, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(tree.Writable()[0], name)
	if err := tree.Write(context.Background(), path, data); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// pngBytes encodes a w×h image; a uniform fill keeps the file small.
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestReadFileReturnsAnImagePart(t *testing.T) {
	fileTools, tree := imageGroup(t, true)
	path := put(t, tree, "pic.png", pngBytes(t, 8, 4))

	content, err := callContent(t, find(t, fileTools, ReadFileName), map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(content.Parts) != 2 {
		t.Fatalf("parts = %d, want an image and a note", len(content.Parts))
	}
	if _, ok := content.Parts[0].(message.ImagePart); !ok {
		t.Fatalf("part 0 = %T, want an image part", content.Parts[0])
	}
	if note := content.Text(); !strings.Contains(note, "8x4") {
		t.Errorf("note %q does not carry the dimensions", note)
	}
}

// Detection is by content: a PNG named .txt is still an image, so it is refused
// as an image rather than read as text.
func TestReadFileDetectsImagesByContent(t *testing.T) {
	fileTools, tree := imageGroup(t, false)
	path := put(t, tree, "notes.txt", pngBytes(t, 4, 4))

	_, err := call(t, find(t, fileTools, ReadFileName), map[string]any{"file_path": path})
	if err == nil {
		t.Fatal("png bytes with a .txt name were accepted")
	}
	if !strings.Contains(err.Error(), "image/png") {
		t.Errorf("error %q does not name the detected type", err)
	}
}

func TestReadFileRefusesImagesWhenDisabled(t *testing.T) {
	fileTools, tree := imageGroup(t, false)
	path := put(t, tree, "pic.png", pngBytes(t, 4, 4))

	_, err := call(t, find(t, fileTools, ReadFileName), map[string]any{"file_path": path})
	if err == nil {
		t.Fatal("image accepted with images disabled")
	}
	if !strings.Contains(err.Error(), "does not accept image input") {
		t.Errorf("error %q does not explain the refusal", err)
	}
}

// A recognised-but-undecodable format is refused by name, not silently read as
// text: this build carries no webp decoder.
func TestReadFileRefusesUndecodableImage(t *testing.T) {
	fileTools, tree := imageGroup(t, true)
	webp := append([]byte("RIFF\x24\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{0}, 32)...)
	path := put(t, tree, "pic.webp", webp)

	_, err := call(t, find(t, fileTools, ReadFileName), map[string]any{"file_path": path})
	if err == nil {
		t.Fatal("webp accepted without a decoder")
	}
	if !strings.Contains(err.Error(), "image/webp") {
		t.Errorf("error %q does not name the format", err)
	}
}

// Oversized images are refused with the way out, because this build does not
// resize.
func TestReadFileRefusesOversizedImage(t *testing.T) {
	fileTools, tree := imageGroup(t, true)
	path := put(t, tree, "big.png", pngBytes(t, maxImageEdge+1, 8))

	_, err := call(t, find(t, fileTools, ReadFileName), map[string]any{"file_path": path})
	if err == nil {
		t.Fatal("oversized image accepted")
	}
	if !strings.Contains(err.Error(), "downscale") {
		t.Errorf("error %q gives no instruction", err)
	}
}
