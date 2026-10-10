// Package imageutil normalizes images before they are persisted and
// inlined into model prompts: EXIF orientation is applied,
// transparency is flattened onto white, and the result is re-encoded
// as JPEG so prompt bytes stay predictable regardless of the source
// format.
package imageutil

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"io"
	"os"

	"github.com/disintegration/imaging"
)

// MaxInlineImageBytes is the shared per-image inline budget. Every path
// that carries an image into a prompt — the media.attachments prepare
// hook today, host-side attachment persistence and data-URL previews in
// applications — enforces the same cap, so "attaches today" stays true
// all the way into the model prompt.
const MaxInlineImageBytes = 10 << 20

// MaxDecodePixels bounds the pixel budget of images that are fully
// decoded for normalization. Decoding a huge-but-compressed image (a
// decompression bomb) would otherwise allocate hundreds of MB per
// working buffer.
const MaxDecodePixels = 40_000_000

// Prompt-side downscale targets. A tool result is replayed in every
// later turn's context, so an image handed to the model is bounded on
// both axes: longest edge in pixels and size in bytes. The edge target
// matches what the wire providers document (~1568 px).
//
// The byte target is raw JPEG size, while the part budget applied to
// non-text tool output is metered on the canonical wire encoding
// (message.MarshalPart): an inline image spends base64-expanded bytes
// (~4/3 of the raw size) plus a small JSON envelope.
// DefaultPromptImageBytes is therefore the raw size that still fits the
// default 1 MiB part budget
// (core/tool/middleware.DefaultResultPartBudget); a wiring test in this
// package pins the two constants against each other.
const (
	// DefaultPromptImageEdge is the longest-edge target in pixels.
	DefaultPromptImageEdge = 1568
	// DefaultPromptImageBytes is the raw-byte target; the marshalled
	// image part stays under the default 1 MiB part budget.
	DefaultPromptImageBytes = 786_000
)

// jpegQualities is the ladder DownscaleToJPEG walks before it scales
// the image down again: quality first (cheap, no resolution loss),
// then dimensions.
var jpegQualities = []int{90, 80, 70, 60, 45}

// JPEGQuality is the quality used for normalized attachment images.
const JPEGQuality = 90

// NormalizeToJPEG decodes r (JPEG/PNG/GIF/TIFF/BMP with EXIF
// orientation applied), flattens transparency onto white, and
// re-encodes the result as a JPEG at JPEGQuality. Formats the decoder
// does not understand (for example WebP/AVIF) return an error so
// callers can fall back to the original bytes. The pixel budget guard
// lives in NormalizeFileToJPEG, which is the production entry point;
// this reader variant is used by tests and callers that already
// validated the input.
func NormalizeToJPEG(r io.Reader) ([]byte, error) {
	img, err := imaging.Decode(r, imaging.AutoOrientation(true))
	if err != nil {
		return nil, err
	}
	img = flattenAlpha(img)
	var out bytes.Buffer
	if err := imaging.Encode(
		&out, img, imaging.JPEG, imaging.JPEGQuality(JPEGQuality),
	); err != nil {
		return nil, fmt.Errorf("imageutil: encode jpeg: %w", err)
	}
	return out.Bytes(), nil
}

// DownscaleToJPEG decodes r (same formats and EXIF handling as
// NormalizeToJPEG) and re-encodes it as a JPEG that fits both maxEdge
// (longest side, pixels; <= 0 keeps the source size) and maxBytes
// (encoded size; <= 0 keeps the smallest quality). Quality is
// stepped down first and the image is scaled further only when the
// lowest quality still does not fit, so a normal screenshot keeps its
// resolution and a huge one still arrives. It returns the encoded
// bytes and the dimensions that were actually encoded.
func DownscaleToJPEG(
	r io.Reader, maxEdge, maxBytes int,
) (out []byte, width, height int, err error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("imageutil: read source: %w", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("imageutil: decode config: %w", err)
	}
	if cfg.Width*cfg.Height > MaxDecodePixels {
		return nil, 0, 0, fmt.Errorf(
			"imageutil: image is %dx%d pixels, over the %d-pixel budget",
			cfg.Width, cfg.Height, MaxDecodePixels,
		)
	}
	img, err := imaging.Decode(bytes.NewReader(raw), imaging.AutoOrientation(true))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("imageutil: decode: %w", err)
	}
	img = flattenAlpha(img)
	bounds := img.Bounds()
	width, height = bounds.Dx(), bounds.Dy()
	if maxEdge > 0 && (width > maxEdge || height > maxEdge) {
		img = imaging.Fit(img, maxEdge, maxEdge, imaging.Lanczos)
		bounds = img.Bounds()
		width, height = bounds.Dx(), bounds.Dy()
	}
	for range 6 {
		for _, quality := range jpegQualities {
			var buf bytes.Buffer
			if err := imaging.Encode(
				&buf, img, imaging.JPEG, imaging.JPEGQuality(quality),
			); err != nil {
				return nil, 0, 0, fmt.Errorf(
					"imageutil: encode jpeg: %w", err,
				)
			}
			out = buf.Bytes()
			if maxBytes <= 0 || len(out) <= maxBytes {
				return out, width, height, nil
			}
		}
		// Every quality is over budget: trade resolution for size.
		if width <= 64 && height <= 64 {
			return out, width, height, nil
		}
		img = imaging.Resize(
			img,
			max(1, width*3/4),
			max(1, height*3/4),
			imaging.Lanczos,
		)
		bounds = img.Bounds()
		width, height = bounds.Dx(), bounds.Dy()
	}
	return out, width, height, nil
}

// NormalizeFileToJPEG is NormalizeToJPEG for a local path. It checks
// the pixel budget before decoding, so a sparse image with enormous
// dimensions is rejected without a full-size allocation.
func NormalizeFileToJPEG(path string) (out []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return nil, err
	}
	pixels := int64(cfg.Width) * int64(cfg.Height)
	if cfg.Width <= 0 || cfg.Height <= 0 || pixels > MaxDecodePixels {
		return nil, fmt.Errorf(
			"imageutil: %s is %dx%d and exceeds the %d-pixel decode limit",
			path, cfg.Width, cfg.Height, MaxDecodePixels)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("imageutil: rewind %s: %w", path, err)
	}
	out, err = NormalizeToJPEG(f)
	return out, err
}

// flattenAlpha replaces transparency with white. JPEG cannot carry an
// alpha channel, and leaving transparent pixels black turns PNG icons
// and screenshots illegible.
func flattenAlpha(src image.Image) image.Image {
	if alphaFree(src) {
		return src
	}
	b := src.Bounds()
	canvas := imaging.New(b.Dx(), b.Dy(), color.White)
	return imaging.Overlay(canvas, src, image.Pt(-b.Min.X, -b.Min.Y), 1)
}

// alphaFree reports whether src is known not to carry transparency, so
// flattenAlpha can skip the extra full-size compositing pass.
func alphaFree(src image.Image) bool {
	switch m := src.(type) {
	case *image.YCbCr, *image.CMYK, *image.Gray, *image.Gray16:
		return true
	case *image.RGBA:
		return m.Opaque()
	case *image.NRGBA:
		return m.Opaque()
	case *image.Paletted:
		for _, c := range m.Palette {
			if _, _, _, a := c.RGBA(); a != 0xffff {
				return false
			}
		}
		return true
	default:
		// Unknown concrete type: composite conservatively.
		return false
	}
}
