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
	"reflect"
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
	plainPath := writeJPEG(t, "plain.jpg", jpegWithOrientation(t, 40, 30, 0))
	if !JPEGUpright(plainPath) {
		t.Fatal("JPEG without EXIF must count as upright")
	}

	rotatedPath := writeJPEG(t, "rotated.jpg", jpegWithOrientation(t, 40, 30, 6))
	if JPEGUpright(rotatedPath) {
		t.Fatal("JPEG with EXIF orientation 6 must need normalization")
	}

	// Normalizing applies the orientation: 40x30 with orientation 6
	// comes back 30x40.
	data, err := NormalizeFileToJPEG(rotatedPath)
	if err != nil {
		t.Fatal(err)
	}
	assertDimensions(t, data, 30, 40)
}

// TestJPEGUprightMirrorsImaging pins JPEGUpright against the reader it
// has to agree with, over the well-formed EXIF shapes a writer
// produces. The oracle decodes the same bytes twice — once with imaging
// applying the orientation — and compares the pixels, so the cases need
// no hand-written table of EXIF semantics and "upright" keeps meaning
// "imaging would not rotate this file".
func TestJPEGUprightMirrorsImaging(t *testing.T) {
	short := func(order binary.ByteOrder, orientation uint32) []byte {
		return app1JPEG(t, exifPayload(exifShort(order, orientation)), 0)
	}
	fixtures := map[string][]byte{
		"no exif":         gradientJPEG(t, 40, 30),
		"little-endian 1": short(binary.LittleEndian, 1),
		"little-endian 6": short(binary.LittleEndian, 6),
		"big-endian 1":    short(binary.BigEndian, 1),
		"big-endian 6":    short(binary.BigEndian, 6),
		"orientation 0":   short(binary.LittleEndian, 0),
		"orientation 9":   short(binary.LittleEndian, 9),
		"LONG-typed 6": app1JPEG(t, exifPayload(exifSpec{
			order: binary.LittleEndian, fieldType: 4, count: 1, value: 6,
		}), 0),
		"count 2": app1JPEG(t, exifPayload(exifSpec{
			order: binary.LittleEndian, fieldType: 3, count: 2, value: 6,
		}), 0),
		"non-EXIF APP1": insertSegment(t, gradientJPEG(t, 40, 30),
			app1Segment([]byte("<x:xmpmeta/>"), 0)),
		"non-EXIF APP1 before EXIF": insertSegment(t, short(binary.LittleEndian, 6),
			app1Segment([]byte("<x:xmpmeta/>"), 0)),
	}
	for name, data := range fixtures {
		path := writeJPEG(t, strings.ReplaceAll(name, " ", "-")+".jpg", data)
		want := !imagingRotates(t, data)
		if got := JPEGUpright(path); got != want {
			t.Errorf("%s: JPEGUpright = %v, imaging rotation = %v", name, got, !want)
		}
	}
}

// TestJPEGUprightDistrustsMalformedStreams pins the deliberate
// divergence from imaging: where this reader cannot follow the stream —
// marker fill bytes, an EXIF block that runs off the end of the file,
// bytes that are not a marker at all — it answers "needs a transform"
// instead of guessing. imaging finds no orientation in those bytes, so
// the cost of the divergence is a needless re-encode.
func TestJPEGUprightDistrustsMalformedStreams(t *testing.T) {
	segment := app1Segment(exifPayload(exifShort(binary.LittleEndian, 6)), 0)
	fixtures := map[string][]byte{
		"marker fill bytes": insertSegment(t, gradientJPEG(t, 40, 30),
			append([]byte{0xff, 0xff}, segment...)),
		"not a marker": insertSegment(t, gradientJPEG(t, 40, 30),
			append([]byte{0x00}, segment...)),
		// The file is an SOI and the segment, cut inside the EXIF block
		// the segment declared: the parse runs off the end of the
		// metadata with no further bytes to misread.
		"truncated EXIF": append([]byte{0xff, 0xd8}, segment[:len(segment)-6]...),
	}
	for name, data := range fixtures {
		path := writeJPEG(t, strings.ReplaceAll(name, " ", "-")+".jpg", data)
		if JPEGUpright(path) {
			t.Errorf("%s: JPEGUpright = true, want a transform", name)
		}
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
	assertDimensions(t, out, width, height)
}

func TestDownscaleToJPEGRejectsHugePixelBudget(t *testing.T) {
	// The header is checked before the full decode, so the bomb is
	// rejected without allocating the huge buffers a decode would.
	_, _, _, err := DownscaleToJPEG(
		bytes.NewReader(hugePNGHeader(20000, 20000)), 0, 0,
	)
	if err == nil || !strings.Contains(err.Error(), "decode limit") {
		t.Fatalf("pixel budget error = %v", err)
	}
}

func TestDownscaleToJPEGReportsTheBytesItReturns(t *testing.T) {
	// A byte budget no encoding can meet: the quality ladder and the
	// resize rounds both run out, and the dimensions that come back
	// still have to describe the bytes that come back.
	src := noisePNG(t, 400, 300)
	out, width, height, err := DownscaleToJPEG(bytes.NewReader(src), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertJPEG(t, out)
	assertDimensions(t, out, width, height)
	if width >= 400 || height >= 300 {
		t.Fatalf("dimensions = %dx%d; the budget must force a downscale",
			width, height)
	}
}

func TestNormalizeToJPEGDecodesTIFFAndBMP(t *testing.T) {
	// TIFF and BMP decode because imaging imports golang.org/x/image
	// decoders for registration; this test is what keeps that implicit
	// dependency from going unnoticed.
	src := gradientImage(40, 30)
	for name, format := range map[string]imaging.Format{
		"BMP":  imaging.BMP,
		"TIFF": imaging.TIFF,
	} {
		var encoded bytes.Buffer
		if err := imaging.Encode(&encoded, src, format); err != nil {
			t.Fatalf("encode %s: %v", name, err)
		}
		out, err := NormalizeToJPEG(bytes.NewReader(encoded.Bytes()))
		if err != nil {
			t.Fatalf("NormalizeToJPEG(%s): %v", name, err)
		}
		assertJPEG(t, out)
		assertDimensions(t, out, 40, 30)
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

// gradientImage draws a smooth, asymmetric RGB gradient: predictable
// JPEG sizes at every quality keep the byte-budget assertions stable,
// and every orientation transform moves its pixels, which is what the
// EXIF tests compare.
func gradientImage(width, height int) *image.NRGBA {
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
	return img
}

// gradientPNG encodes gradientImage as PNG.
func gradientPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, gradientImage(width, height)); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

// gradientJPEG encodes gradientImage as JPEG: the base every EXIF
// fixture splices its metadata segment into.
func gradientJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := imaging.Encode(
		&encoded, gradientImage(width, height), imaging.JPEG,
		imaging.JPEGQuality(JPEGQuality),
	); err != nil {
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

// jpegWithOrientation returns a gradient JPEG of the given size with one
// little-endian EXIF orientation tag, and no APP1 at all for
// orientation <= 1.
func jpegWithOrientation(t *testing.T, width, height, orientation int) []byte {
	t.Helper()
	raw := gradientJPEG(t, width, height)
	if orientation <= 1 {
		return raw
	}
	return insertSegment(t, raw, app1Segment(
		exifPayload(exifShort(binary.LittleEndian, uint32(orientation))), 0))
}

// app1JPEG splices one APP1 segment into a fresh 40x30 gradient JPEG.
func app1JPEG(t *testing.T, payload []byte, size int) []byte {
	t.Helper()
	return insertSegment(t, gradientJPEG(t, 40, 30), app1Segment(payload, size))
}

// exifSpec describes one orientation entry the way a writer may have
// stored it: the byte order, the declared field type and count, and the
// value that goes into the 4-byte value field.
type exifSpec struct {
	order     binary.ByteOrder
	fieldType uint16
	count     uint32
	value     uint32
}

// exifShort is the spec-conforming shape: a SHORT with one value.
func exifShort(order binary.ByteOrder, orientation uint32) exifSpec {
	return exifSpec{
		order: order, fieldType: 3, count: 1, value: orientation,
	}
}

// exifPayload builds an EXIF APP1 payload holding exactly one IFD0
// entry.
func exifPayload(spec exifSpec) []byte {
	var exif bytes.Buffer
	exif.WriteString("Exif\x00\x00")
	if spec.order == binary.BigEndian {
		exif.WriteString("MM")
	} else {
		exif.WriteString("II")
	}
	_ = binary.Write(&exif, spec.order, uint16(0x002a))
	_ = binary.Write(&exif, spec.order, uint32(8)) // IFD0 offset
	_ = binary.Write(&exif, spec.order, uint16(1)) // one entry
	_ = binary.Write(&exif, spec.order, uint16(0x0112))
	_ = binary.Write(&exif, spec.order, spec.fieldType)
	_ = binary.Write(&exif, spec.order, spec.count)
	if spec.fieldType == 3 {
		// A SHORT sits in the first two bytes of the value field.
		_ = binary.Write(&exif, spec.order, uint16(spec.value))
		exif.Write([]byte{0, 0})
	} else {
		_ = binary.Write(&exif, spec.order, spec.value)
	}
	return exif.Bytes()
}

// app1Segment wraps payload in an APP1 segment. A positive size is the
// declared length, so a segment can claim a length it does not carry.
func app1Segment(payload []byte, size int) []byte {
	if size <= 0 {
		size = len(payload) + 2
	}
	segment := []byte{0xff, 0xe1, byte(size >> 8), byte(size)}
	return append(segment, payload...)
}

// insertSegment splices one JPEG segment right after the SOI marker.
func insertSegment(t *testing.T, jpegData, segment []byte) []byte {
	t.Helper()
	if len(jpegData) < 2 || jpegData[0] != 0xff || jpegData[1] != 0xd8 {
		t.Fatal("input is not a JPEG")
	}
	out := make([]byte, 0, len(jpegData)+len(segment))
	out = append(out, jpegData[:2]...)
	out = append(out, segment...)
	out = append(out, jpegData[2:]...)
	return out
}

// writeJPEG writes one fixture to a temp dir and returns its path.
func writeJPEG(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertDimensions decodes a JPEG and checks the pixel dimensions it
// actually carries, which are the ones DownscaleToJPEG reports back.
func assertDimensions(t *testing.T, data []byte, width, height int) {
	t.Helper()
	img, err := imaging.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotWidth, gotHeight := imageSize(img); gotWidth != width || gotHeight != height {
		t.Fatalf("encoded dimensions = %dx%d, reported %dx%d",
			gotWidth, gotHeight, width, height)
	}
}

// imagingRotates reports whether imaging's AutoOrientation decode
// differs from a plain decode of the same bytes: the oracle JPEGUpright
// has to agree with, because imaging is what re-encodes a file reported
// as needing a transform.
func imagingRotates(t *testing.T, data []byte) bool {
	t.Helper()
	plain, err := imaging.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	oriented, err := imaging.Decode(
		bytes.NewReader(data), imaging.AutoOrientation(true))
	if err != nil {
		t.Fatalf("decode with orientation: %v", err)
	}
	return !reflect.DeepEqual(imaging.Clone(plain), imaging.Clone(oriented))
}
