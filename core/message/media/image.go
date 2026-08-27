package media

import (
	"fmt"
	"strconv"
	"strings"
)

type ImageSize struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (s ImageSize) Validate() error {
	if s.Width <= 0 || s.Height <= 0 {
		return fmt.Errorf("image dimensions must be positive")
	}
	return nil
}

type AspectRatio string

func (r AspectRatio) Validate() error {
	width, height, ok := strings.Cut(string(r), ":")
	if !ok {
		return fmt.Errorf("aspect ratio must use width:height")
	}
	w, wErr := strconv.Atoi(width)
	h, hErr := strconv.Atoi(height)
	if wErr != nil || hErr != nil || w <= 0 || h <= 0 {
		return fmt.Errorf("aspect ratio components must be positive integers")
	}
	return nil
}

type ImageFormat string

const (
	ImageFormatPNG  ImageFormat = "png"
	ImageFormatJPEG ImageFormat = "jpeg"
	ImageFormatWebP ImageFormat = "webp"
	ImageFormatGIF  ImageFormat = "gif"
	ImageFormatAVIF ImageFormat = "avif"
)

func (f ImageFormat) Validate() error {
	switch f {
	case ImageFormatPNG, ImageFormatJPEG, ImageFormatWebP, ImageFormatGIF,
		ImageFormatAVIF:
		return nil
	default:
		return fmt.Errorf("unknown image format %q", f)
	}
}

func (f ImageFormat) MediaType() string {
	switch f {
	case ImageFormatPNG:
		return "image/png"
	case ImageFormatJPEG:
		return "image/jpeg"
	case ImageFormatWebP:
		return "image/webp"
	case ImageFormatGIF:
		return "image/gif"
	case ImageFormatAVIF:
		return "image/avif"
	default:
		return ""
	}
}

// ImageQuality is the canonical generation quality tier. The values mirror
// the gpt-image family's quality parameter — the only image provider that
// exposes a per-request quality knob. Providers without a native quality
// parameter reject the field at compile time rather than approximate it.
type ImageQuality string

const (
	// ImageQualityAuto defers to the provider's default quality.
	ImageQualityAuto ImageQuality = "auto"
	// ImageQualityLow trades quality for speed.
	ImageQualityLow ImageQuality = "low"
	// ImageQualityMedium is the middle quality tier.
	ImageQualityMedium ImageQuality = "medium"
	// ImageQualityHigh is the best quality tier.
	ImageQualityHigh ImageQuality = "high"
)

func (q ImageQuality) Validate() error {
	switch q {
	case ImageQualityAuto, ImageQualityLow, ImageQualityMedium, ImageQualityHigh:
		return nil
	default:
		return fmt.Errorf("unknown image quality %q", q)
	}
}
