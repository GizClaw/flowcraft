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
	if fields := (ImageOptions{Mask: &mask}).ActiveFields(); len(fields) != 1 ||
		string(fields[0]) != "mask" {
		t.Fatalf("mask ActiveFields = %#v, want [mask]", fields)
	}
	two := 2
	if fields := (ImageOptions{PartialImages: &two}).ActiveFields(); len(fields) != 1 ||
		string(fields[0]) != "partial_images" {
		t.Fatalf("partial_images ActiveFields = %#v, want [partial_images]", fields)
	}
	if fields := (ImageOptions{Mask: &mask, PartialImages: &two}).ActiveFields(); len(fields) != 2 {
		t.Fatalf("combined ActiveFields = %#v, want [mask partial_images]", fields)
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

func TestImageOptionsValidatePartialImages(t *testing.T) {
	for _, count := range []int{0, 1, 2, 3} {
		value := count
		if err := (ImageOptions{PartialImages: &value}).Validate(); err != nil {
			t.Errorf("partial_images=%d Validate() = %v, want nil", count, err)
		}
	}
	for _, count := range []int{-1, 4, 9} {
		value := count
		err := (ImageOptions{PartialImages: &value}).Validate()
		if err == nil || !strings.Contains(err.Error(), "partial_images") {
			t.Errorf("partial_images=%d Validate() = %v, want range error", count, err)
		}
	}
}

func TestImageOptionsCloneDeepCopiesMask(t *testing.T) {
	mask, err := media.NewImageBytes(testPNG, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	partial := 3
	options := ImageOptions{Mask: &mask, PartialImages: &partial}
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
	if cloned.PartialImages == nil || cloned.PartialImages == options.PartialImages {
		t.Fatal("Clone() must deep-copy partial_images")
	}
	if *cloned.PartialImages != 3 {
		t.Fatalf("Clone() partial_images = %d, want 3", *cloned.PartialImages)
	}
}
