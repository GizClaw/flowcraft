package azure

import (
	"bytes"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message/media"
)

func TestImageOptionsActiveFields(t *testing.T) {
	if fields := (ImageOptions{}).ActiveFields(); len(fields) != 0 {
		t.Fatalf("empty ImageOptions ActiveFields = %#v, want none", fields)
	}
	mask, err := media.NewImageBytes(testPNG, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	fields := (ImageOptions{Mask: &mask}).ActiveFields()
	if len(fields) != 1 || string(fields[0]) != "mask" {
		t.Fatalf("ActiveFields = %#v, want [mask]", fields)
	}
}

func TestImageOptionsValidateMask(t *testing.T) {
	if err := (ImageOptions{}).Validate(); err != nil {
		t.Fatalf("empty Validate() = %v, want nil", err)
	}
	png, err := media.NewImageBytes(testPNG, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	if err := (ImageOptions{Mask: &png}).Validate(); err != nil {
		t.Fatalf("PNG mask Validate() = %v, want nil", err)
	}
	jpeg, err := media.NewImageBytes(testPNG, "image/jpeg")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	err = (ImageOptions{Mask: &jpeg}).Validate()
	if err == nil || !strings.Contains(err.Error(), "PNG") {
		t.Fatalf("JPEG mask Validate() = %v, want PNG constraint", err)
	}
}

func TestImageOptionsCloneDeepCopiesMask(t *testing.T) {
	mask, err := media.NewImageBytes(testPNG, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	options := ImageOptions{Mask: &mask}
	cloned, ok := options.Clone().(ImageOptions)
	if !ok {
		t.Fatalf("Clone() type = %T, want ImageOptions", options.Clone())
	}
	if cloned.Mask == nil || cloned.Mask == options.Mask {
		t.Fatal("Clone() must deep-copy the mask source")
	}
	if !bytes.Equal(cloned.Mask.Bytes(), options.Mask.Bytes()) {
		t.Fatal("Clone() lost the mask bytes")
	}
}
