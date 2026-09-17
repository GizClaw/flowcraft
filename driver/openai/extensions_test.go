package openai

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
	two := 2
	fields := (ImageOptions{PartialImages: &two}).ActiveFields()
	if len(fields) != 1 || string(fields[0]) != "partial_images" {
		t.Fatalf("ActiveFields = %#v, want [partial_images]", fields)
	}
	mask, err := media.NewImageBytes([]byte("png"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	fields = (ImageOptions{Mask: &mask}).ActiveFields()
	if len(fields) != 1 || string(fields[0]) != "mask" {
		t.Fatalf("ActiveFields = %#v, want [mask]", fields)
	}
}

func TestImageOptionsValidateMask(t *testing.T) {
	png, err := media.NewImageBytes([]byte("png"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	if err := (ImageOptions{Mask: &png}).Validate(); err != nil {
		t.Errorf("PNG mask Validate() = %v, want nil", err)
	}
	jpeg, err := media.NewImageBytes([]byte("jpeg"), "image/jpeg")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	err = (ImageOptions{Mask: &jpeg}).Validate()
	if err == nil || !strings.Contains(err.Error(), "PNG") {
		t.Errorf("JPEG mask Validate() = %v, want PNG error", err)
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

func TestImageOptionsCloneDeepCopiesPartialImages(t *testing.T) {
	three := 3
	options := ImageOptions{PartialImages: &three}
	cloned, ok := options.Clone().(ImageOptions)
	if !ok {
		t.Fatalf("Clone() type = %T, want ImageOptions", options.Clone())
	}
	if cloned.PartialImages == nil || cloned.PartialImages == options.PartialImages {
		t.Fatal("Clone() must deep-copy partial_images")
	}
	if *cloned.PartialImages != 3 {
		t.Fatalf("Clone() partial_images = %d, want 3", *cloned.PartialImages)
	}
}

func TestImageOptionsCloneDeepCopiesMask(t *testing.T) {
	mask, err := media.NewImageBytes([]byte("png"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	options := ImageOptions{Mask: &mask}
	cloned, ok := options.Clone().(ImageOptions)
	if !ok {
		t.Fatalf("Clone() type = %T, want ImageOptions", options.Clone())
	}
	if cloned.Mask == nil || cloned.Mask == options.Mask {
		t.Fatal("Clone() must deep-copy mask")
	}
	if !bytes.Equal(cloned.Mask.Bytes(), mask.Bytes()) ||
		cloned.Mask.BaseMediaType() != "image/png" {
		t.Fatalf("Clone() mask = %+v, want the same PNG bytes", cloned.Mask)
	}
}
