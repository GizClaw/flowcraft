package imageutil

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/disintegration/imaging"
)

func TestNormalizeToJPEGFlattensTransparency(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 40, 30))
	for y := 0; y < 30; y++ {
		for x := 0; x < 40; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}
	// Leave one corner transparent so the alpha path is exercised.
	src.SetNRGBA(0, 0, color.NRGBA{})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, src); err != nil {
		t.Fatal(err)
	}

	data, err := NormalizeToJPEG(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(data, []byte{0xff, 0xd8}) {
		t.Fatalf("output is not a JPEG: %x", data[:min(len(data), 4)])
	}
	img, err := imaging.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx() != 40 || b.Dy() != 30 {
		t.Fatalf("dimensions = %dx%d, want 40x30", b.Dx(), b.Dy())
	}
	nrgba, ok := img.(*image.NRGBA)
	if ok && !nrgba.Opaque() {
		t.Fatal("jpeg output still carries transparency")
	}
}

func TestNormalizeToJPEGRejectsUnsupportedFormat(t *testing.T) {
	if _, err := NormalizeToJPEG(strings.NewReader("not an image")); err == nil {
		t.Fatal("unsupported input unexpectedly normalized")
	}
}

func TestNormalizeFileToJPEGRejectsHugePixelBudget(t *testing.T) {
	// A sparse PNG whose header advertises enormous dimensions must be
	// rejected before any full-size allocation happens.
	src := filepath.Join(t.TempDir(), "bomb.png")
	if err := os.WriteFile(src, hugePNGHeader(20000, 20000), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := NormalizeFileToJPEG(src); err == nil ||
		!strings.Contains(err.Error(), "decode limit") {
		t.Fatalf("pixel budget error = %v", err)
	}
}

func TestJPEGUprightAndExifRotation(t *testing.T) {
	plain := jpegWithOrientation(t, 40, 30, 0)
	plainPath := filepath.Join(t.TempDir(), "plain.jpg")
	if err := os.WriteFile(plainPath, plain, 0o600); err != nil {
		t.Fatal(err)
	}
	if !JPEGUpright(plainPath) {
		t.Fatal("JPEG without EXIF must count as upright")
	}

	rotated := jpegWithOrientation(t, 40, 30, 6)
	rotatedPath := filepath.Join(t.TempDir(), "rotated.jpg")
	if err := os.WriteFile(rotatedPath, rotated, 0o600); err != nil {
		t.Fatal(err)
	}
	if JPEGUpright(rotatedPath) {
		t.Fatal("JPEG with EXIF orientation 6 must need normalization")
	}

	// Normalizing applies the orientation: 40x30 with orientation 6
	// comes back 30x40.
	data, err := NormalizeFileToJPEG(rotatedPath)
	if err != nil {
		t.Fatal(err)
	}
	img, err := imaging.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := img.Bounds()
	if b.Dx() != 30 || b.Dy() != 40 {
		t.Fatalf("normalized dimensions = %dx%d, want 30x40", b.Dx(), b.Dy())
	}
}

func TestDownscaleToJPEGCapsTheLongEdge(t *testing.T) {
	src := gradientPNG(t, 320, 240)
	out, width, height, err := DownscaleToJPEG(
		bytes.NewReader(src), 100, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if width != 100 || height != 75 {
		t.Fatalf("dimensions = %dx%d, want 100x75", width, height)
	}
	assertJPEG(t, out)
	img, err := imaging.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 100 || b.Dy() != 75 {
		t.Fatalf("encoded dimensions = %dx%d, want 100x75", b.Dx(), b.Dy())
	}
}

func TestDownscaleToJPEGSpendsQualityBeforeResolution(t *testing.T) {
	// A smooth gradient keeps its resolution when only the byte budget
	// is tight: q90 exceeds the budget (by design of this fixture), so
	// stepping quality down is enough to fit.
	src := gradientPNG(t, 320, 240)
	out, width, height, err := DownscaleToJPEG(
		bytes.NewReader(src), 0, 5_000,
	)
	if err != nil {
		t.Fatal(err)
	}
	if width != 320 || height != 240 {
		t.Fatalf("dimensions = %dx%d; the quality ladder should absorb the budget",
			width, height)
	}
	assertJPEG(t, out)
	if len(out) > 5_000 {
		t.Fatalf("encoded size = %d, over the 5000-byte budget", len(out))
	}
}

func TestDownscaleToJPEGTradesResolutionForSize(t *testing.T) {
	// Noise defeats the quality ladder, so the budget is met by scaling
	// the image down: the result is a smaller, valid JPEG.
	src := noisePNG(t, 320, 240)
	out, width, height, err := DownscaleToJPEG(
		bytes.NewReader(src), 0, 4_000,
	)
	if err != nil {
		t.Fatal(err)
	}
	assertJPEG(t, out)
	if width >= 320 || height >= 240 {
		t.Fatalf("dimensions = %dx%d; the byte budget must force a downscale",
			width, height)
	}
	if len(out) > 4_000 && width > 64 && height > 64 {
		t.Fatalf("over budget (%d bytes) at %dx%d without reaching the floor",
			len(out), width, height)
	}
}

func TestDownscaleToJPEGRejectsHugePixelBudget(t *testing.T) {
	// The header is checked before the full decode, so the bomb is
	// rejected without allocating the huge buffers a decode would.
	_, _, _, err := DownscaleToJPEG(
		bytes.NewReader(hugePNGHeader(20000, 20000)), 0, 0,
	)
	if err == nil || !strings.Contains(err.Error(), "pixel budget") {
		t.Fatalf("pixel budget error = %v", err)
	}
}

// hugePNGHeader builds a PNG whose IHDR advertises the given
// dimensions with no pixel data behind it: what a decompression bomb
// looks like to a header-only check.
func hugePNGHeader(width, height int) []byte {
	var ihdr bytes.Buffer
	_ = binary.Write(&ihdr, binary.BigEndian, uint32(width))
	_ = binary.Write(&ihdr, binary.BigEndian, uint32(height))
	ihdr.WriteByte(8) // bit depth
	ihdr.WriteByte(2) // RGB, no alpha
	ihdr.Write([]byte{0, 0, 0})
	var pngFile bytes.Buffer
	pngFile.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	_ = binary.Write(&pngFile, binary.BigEndian, uint32(ihdr.Len()))
	pngFile.WriteString("IHDR")
	pngFile.Write(ihdr.Bytes())
	_ = binary.Write(&pngFile, binary.BigEndian,
		crc32.ChecksumIEEE(append([]byte("IHDR"), ihdr.Bytes()...)))
	return pngFile.Bytes()
}

func assertJPEG(t *testing.T, data []byte) {
	t.Helper()
	if !bytes.HasPrefix(data, []byte{0xff, 0xd8}) {
		t.Fatalf("output is not a JPEG: %x", data[:min(len(data), 4)])
	}
}

// gradientPNG encodes a smooth RGB gradient: predictable JPEG sizes at
// every quality, which keeps the byte-budget assertions stable.
func gradientPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(x * 255 / max(width-1, 1)),
				G: uint8(y * 255 / max(height-1, 1)),
				B: 128,
				A: 255,
			})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

// noisePNG encodes a deterministic LCG noise field: high entropy that no
// JPEG quality can compress, so only resolution carries the byte budget.
func noisePNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	state := uint32(1)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			state = state*1664525 + 1013904223
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(state >> 24),
				G: uint8(state >> 16),
				B: uint8(state >> 8),
				A: 255,
			})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func jpegWithOrientation(t *testing.T, width, height, orientation int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 255, A: 255})
		}
	}
	var raw bytes.Buffer
	if err := imaging.Encode(
		&raw, img, imaging.JPEG, imaging.JPEGQuality(JPEGQuality),
	); err != nil {
		t.Fatal(err)
	}
	if orientation <= 1 {
		return raw.Bytes()
	}
	return insertExifOrientation(t, raw.Bytes(), orientation)
}

// insertExifOrientation splices one little-endian APP1 segment with an
// orientation tag right after the JPEG SOI marker.
func insertExifOrientation(t *testing.T, jpegData []byte, orientation int) []byte {
	t.Helper()
	if len(jpegData) < 2 || jpegData[0] != 0xff || jpegData[1] != 0xd8 {
		t.Fatal("input is not a JPEG")
	}
	var exif bytes.Buffer
	exif.WriteString("Exif\x00\x00")
	exif.WriteString("II")
	_ = binary.Write(&exif, binary.LittleEndian, uint16(0x002a))
	_ = binary.Write(&exif, binary.LittleEndian, uint32(8)) // IFD0 offset
	_ = binary.Write(&exif, binary.LittleEndian, uint16(1)) // one tag
	_ = binary.Write(&exif, binary.LittleEndian, uint16(0x0112))
	_ = binary.Write(&exif, binary.LittleEndian, uint16(3)) // SHORT
	_ = binary.Write(&exif, binary.LittleEndian, uint32(1)) // count
	_ = binary.Write(&exif, binary.LittleEndian, uint32(orientation))
	payload := exif.Bytes()
	app1 := []byte{0xff, 0xe1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	out := make([]byte, 0, len(jpegData)+len(app1)+len(payload))
	out = append(out, jpegData[:2]...)
	out = append(out, app1...)
	out = append(out, payload...)
	out = append(out, jpegData[2:]...)
	return out
}
