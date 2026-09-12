package inference

import (
	"bytes"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func streamImagePart(t *testing.T, data []byte) message.ImagePart {
	t.Helper()
	source, err := media.NewImageBytes(data, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	return message.ImagePart{Source: source}
}

// TestImagePartAccumulatorInterimSnapshots guards the progress-snapshot
// semantics of ImagePartDelta: interim deltas replace the previous image at
// the same part index, and the terminal delta replaces the last snapshot.
func TestImagePartAccumulatorInterimSnapshots(t *testing.T) {
	previewA := streamImagePart(t, []byte("preview-a"))
	previewB := streamImagePart(t, []byte("preview-b"))
	final := streamImagePart(t, []byte("final"))

	acc := &generatePartAccumulator{kind: message.PartImage}
	if err := acc.add(ImagePartDelta{Part: previewA, Interim: true}); err != nil {
		t.Fatalf("add interim preview A: %v", err)
	}
	if err := acc.add(ImagePartDelta{Part: previewB, Interim: true}); err != nil {
		t.Fatalf("add interim preview B: %v", err)
	}
	if err := acc.add(ImagePartDelta{Part: final}); err != nil {
		t.Fatalf("add final image: %v", err)
	}
	part, err := acc.result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	image, ok := part.(message.ImagePart)
	if !ok {
		t.Fatalf("result part = %T, want ImagePart", part)
	}
	if got := image.Source.Bytes(); !bytes.Equal(got, []byte("final")) {
		t.Fatalf("result image bytes = %q, want final image", got)
	}
}

// TestImagePartAccumulatorRejectsDuplicateFinal guards the terminal-image
// rule: a part index that already holds a final image rejects another final
// delta, while interim snapshots may keep replacing it.
func TestImagePartAccumulatorRejectsDuplicateFinal(t *testing.T) {
	first := streamImagePart(t, []byte("first"))
	second := streamImagePart(t, []byte("second"))

	acc := &generatePartAccumulator{kind: message.PartImage}
	if err := acc.add(ImagePartDelta{Part: first}); err != nil {
		t.Fatalf("add first final image: %v", err)
	}
	if err := acc.add(ImagePartDelta{Part: second}); err == nil {
		t.Fatal("second final image at the same part index succeeded, want error")
	}
}

// TestImagePartAccumulatorRejectsInterimTerminal guards the other half of the
// terminal-image rule: a part index whose last delta is a progress snapshot
// has no final image, and returning the snapshot would hand the caller a
// partially rendered image as the result.
func TestImagePartAccumulatorRejectsInterimTerminal(t *testing.T) {
	preview := streamImagePart(t, []byte("preview"))
	final := streamImagePart(t, []byte("final"))

	interimOnly := &generatePartAccumulator{kind: message.PartImage}
	if err := interimOnly.add(ImagePartDelta{Part: preview, Interim: true}); err != nil {
		t.Fatalf("add interim preview: %v", err)
	}
	if _, err := interimOnly.result(); err == nil {
		t.Fatal("a result that ends on an interim snapshot succeeded, want error")
	}

	// A late snapshot must not displace the final image either: the index is
	// still incomplete until a non-interim delta follows it.
	finalThenInterim := &generatePartAccumulator{kind: message.PartImage}
	if err := finalThenInterim.add(ImagePartDelta{Part: final}); err != nil {
		t.Fatalf("add final image: %v", err)
	}
	if err := finalThenInterim.add(ImagePartDelta{Part: preview, Interim: true}); err != nil {
		t.Fatalf("add late interim snapshot: %v", err)
	}
	if _, err := finalThenInterim.result(); err == nil {
		t.Fatal("a late interim snapshot was accepted as the final image")
	}
}
