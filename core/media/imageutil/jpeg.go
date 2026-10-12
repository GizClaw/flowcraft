package imageutil

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"os"
)

const (
	markerSOI      = 0xffd8
	markerAPP1     = 0xffe1
	markerSOS      = 0xffda
	markerEOI      = 0xffd9
	markerFill     = 0xff
	exifSignature  = 0x45786966 // "Exif"
	byteOrderLE    = 0x4949     // "II"
	byteOrderBE    = 0x4d4d     // "MM"
	orientationTag = 0x0112
)

// JPEGUpright reports whether path is a JPEG whose EXIF orientation
// requires no transform: orientation 1, an orientation value outside
// 1..8 that imaging rejects as well, an APP1 segment that is not EXIF,
// or no APP1 segment at all. Such a file can be persisted and served
// byte-for-byte; a file that needs a transform falls back to full decode
// + JPEG normalization.
//
// The reader mirrors imaging's, because imaging is what re-encodes a
// file reported as needing a transform: it looks at the first APP1
// segment only, and takes the orientation from the first two bytes of
// the tag's value field whatever type and count the entry declares.
// Where this reader cannot follow a malformed stream — truncated EXIF
// data, marker fill bytes imaging misreads — it reports "needs a
// transform" instead of guessing: a needless re-encode costs quality,
// a missed rotation ships a sideways image.
func JPEGUpright(path string) (upright bool) {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && upright {
			// A failed close can mean a failed read: treat the
			// orientation as unknown so callers normalize instead of
			// trusting bytes that may be incomplete.
			upright = false
		}
	}()
	return jpegUpright(f)
}

// errStreamEnd reports that the marker walk reached the end of the
// metadata segments with no APP1 among them, which is a file imaging
// applies no orientation to.
var (
	errStreamEnd  = errors.New("no marker left to read")
	errNotAMarker = errors.New("stream is not at a marker")
)

// jpegUpright walks the JPEG markers up to the first APP1, which is
// where imaging's reader stops looking, and reads the orientation out of
// it.
func jpegUpright(r io.Reader) bool {
	br := bufio.NewReader(r)
	var soi uint16
	if err := binary.Read(br, binary.BigEndian, &soi); err != nil ||
		soi != markerSOI {
		return true // not a JPEG; the caller's decode path reports that
	}
	for {
		marker, err := nextMarker(br)
		switch {
		case errors.Is(err, errStreamEnd):
			return true
		case err != nil:
			return false
		}
		if !hasPayload(marker) {
			if marker == markerEOI {
				return true
			}
			continue
		}
		var size uint16
		if err := binary.Read(br, binary.BigEndian, &size); err != nil {
			return true // truncated file: imaging finds no orientation
		}
		if marker == markerAPP1 {
			// imaging ignores the declared length for APP1 and stops
			// here, so the segment is parsed from its own length and the
			// bounded parse decides.
			return app1Upright(br, int64(size)-2)
		}
		if size < 2 || marker == markerSOS {
			// A malformed segment, or the scan data: no metadata segment
			// is left to read, and imaging finds no orientation there.
			return true
		}
		if _, err := io.CopyN(io.Discard, br, int64(size)-2); err != nil {
			return true // truncated file: imaging finds no orientation
		}
	}
}

// nextMarker reads the marker due at the current position. A stream this
// reader cannot follow — a byte that is not a marker prefix, marker fill
// bytes, or a stuffed 0xFF00 outside the scan data — reports
// errNotAMarker, because imaging reads such a stream differently and
// guessing "upright" is the one answer that must not be wrong.
func nextMarker(br *bufio.Reader) (uint16, error) {
	prefix, err := br.ReadByte()
	switch {
	case errors.Is(err, io.EOF):
		return 0, errStreamEnd
	case err != nil:
		return 0, err
	case prefix != markerFill:
		return 0, errNotAMarker
	}
	code, err := br.ReadByte()
	switch {
	case errors.Is(err, io.EOF):
		return 0, errStreamEnd
	case err != nil:
		return 0, err
	case code == 0 || code == markerFill:
		return 0, errNotAMarker
	}
	return 0xFF00 | uint16(code), nil
}

// hasPayload reports whether a marker carries a length field and a
// segment payload. TEM, the restart markers, SOI and EOI stand alone.
func hasPayload(marker uint16) bool {
	if marker == 0xFF01 || (marker >= 0xFFD0 && marker <= 0xFFD9) {
		return false
	}
	return true
}

// app1Upright reads the EXIF orientation tag out of one APP1 segment
// whose payload is size bytes. Parsing is bounded by the segment, so a
// truncated EXIF block reports "needs a transform" rather than walking
// into the bytes that follow it. A block that parses but holds no
// orientation tag reports "upright", which is what imaging finds in the
// same bytes.
func app1Upright(r io.Reader, size int64) bool {
	if size < 0 {
		return false
	}
	segment := io.LimitReader(r, size)
	var signature uint32
	if err := binary.Read(segment, binary.BigEndian, &signature); err != nil {
		return false
	}
	if signature != exifSignature {
		return true // XMP, ICC, …: imaging stops here too
	}
	// The two pad bytes, the byte order, the unchecked 42 magic and the
	// IFD0 offset: the same fields imaging reads in the same order.
	if _, err := io.CopyN(io.Discard, segment, 2); err != nil {
		return false
	}
	var orderTag uint16
	if err := binary.Read(segment, binary.BigEndian, &orderTag); err != nil {
		return false
	}
	var order binary.ByteOrder
	switch orderTag {
	case byteOrderLE:
		order = binary.LittleEndian
	case byteOrderBE:
		order = binary.BigEndian
	default:
		return true // unknown byte order: imaging finds no orientation
	}
	if _, err := io.CopyN(io.Discard, segment, 2); err != nil {
		return false
	}
	var ifd0 uint32
	if err := binary.Read(segment, order, &ifd0); err != nil {
		return false
	}
	if ifd0 < 8 {
		return true // imaging rejects the offset and finds no orientation
	}
	if _, err := io.CopyN(io.Discard, segment, int64(ifd0)-8); err != nil {
		return false
	}
	var entries uint16
	if err := binary.Read(segment, order, &entries); err != nil {
		return false
	}
	// One IFD entry is 12 bytes: tag, field type, count, value field.
	var entry [12]byte
	for range entries {
		if _, err := io.ReadFull(segment, entry[:]); err != nil {
			return false
		}
		if order.Uint16(entry[0:2]) != orientationTag {
			continue
		}
		// imaging reads the first two bytes of the value field as a
		// uint16 and ignores the declared type and count, so an
		// orientation stored as a LONG rotates there and must not be
		// called upright here. 1 is normal, 2..8 transform, and a value
		// imaging rejects — 0, or above 8 — transforms nothing.
		orientation := order.Uint16(entry[8:10])
		return orientation < 2 || orientation > 8
	}
	return true // no orientation tag: imaging applies no transform
}
