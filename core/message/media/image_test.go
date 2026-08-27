package media

import "testing"

func TestImageGeometryAndFormatValidate(t *testing.T) {
	if err := (ImageSize{Width: 1024, Height: 768}).Validate(); err != nil {
		t.Fatalf("ImageSize.Validate: %v", err)
	}
	if err := (ImageSize{Width: 0, Height: 768}).Validate(); err == nil {
		t.Fatal("invalid image size was accepted")
	}
	if err := AspectRatio("16:9").Validate(); err != nil {
		t.Fatalf("AspectRatio.Validate: %v", err)
	}
	if err := AspectRatio("wide").Validate(); err == nil {
		t.Fatal("invalid aspect ratio was accepted")
	}
	if got := ImageFormatPNG.MediaType(); got != "image/png" {
		t.Fatalf("PNG media type = %q", got)
	}
	if err := ImageFormat("bitmap").Validate(); err == nil {
		t.Fatal("unknown image format was accepted")
	}
	for _, quality := range []ImageQuality{
		ImageQualityAuto,
		ImageQualityLow,
		ImageQualityMedium,
		ImageQualityHigh,
	} {
		if err := quality.Validate(); err != nil {
			t.Errorf("ImageQuality(%q).Validate: %v", quality, err)
		}
	}
	if err := ImageQuality("ultra").Validate(); err == nil {
		t.Fatal("unknown image quality was accepted")
	}
}
