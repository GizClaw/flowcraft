package imageutil

import (
	"bufio"
	"encoding/binary"
	"io"
	"os"
)

const (
	markerSOI      = 0xffd8
	markerAPP1     = 0xffe1
	exifSignature  = 0x45786966 // "Exif"
	byteOrderLE    = 0x4949     // "II"
	byteOrderBE    = 0x4d4d     // "MM"
	orientationTag = 0x0112
)

// JPEGUpright reports whether path is a JPEG whose EXIF orientation
// (when present) requires no rotation or flip. Upright JPEGs can be
// persisted and served byte-for-byte; files that need an orientation
// transform fall back to full decode + JPEG q90 normalization. A
// missing or unreadable EXIF block counts as upright, matching how
// imaging.AutoOrientation treats "no orientation metadata".
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

func jpegUpright(r io.Reader) bool {
	br := bufio.NewReader(r)
	var soi uint16
	if err := binary.Read(br, binary.BigEndian, &soi); err != nil ||
		soi != markerSOI {
		return true // not a JPEG; the caller's decode path reports that
	}
	for {
		var marker, size uint16
		if err := binary.Read(br, binary.BigEndian, &marker); err != nil {
			return true
		}
		if marker>>8 != 0xff {
			return true
		}
		if err := binary.Read(br, binary.BigEndian, &size); err != nil {
			return true
		}
		if size < 2 {
			return true
		}
		payload := int64(size - 2)
		if marker == markerAPP1 {
			return app1Upright(br, payload)
		}
		if _, err := io.CopyN(io.Discard, br, payload); err != nil {
			return true
		}
	}
}

// app1Upright parses the EXIF orientation tag out of one APP1
// segment. Every parse problem falls back to "upright" for the same
// reason JPEGUpright documents: imaging would also skip the unknown
// orientation instead of transforming the pixels.
func app1Upright(br *bufio.Reader, payload int64) bool {
	const minAPP1 = 14 // "Exif\0\0" + TIFF header + IFD0 offset
	if payload < minAPP1 {
		return true
	}
	var signature uint32
	if err := binary.Read(br, binary.BigEndian, &signature); err != nil ||
		signature != exifSignature {
		return true
	}
	if _, err := io.CopyN(io.Discard, br, 2); err != nil {
		return true
	}
	var byteOrderTag uint16
	if err := binary.Read(br, binary.BigEndian, &byteOrderTag); err != nil {
		return true
	}
	var order binary.ByteOrder
	switch byteOrderTag {
	case byteOrderLE:
		order = binary.LittleEndian
	case byteOrderBE:
		order = binary.BigEndian
	default:
		return true
	}
	var magic uint16
	if err := binary.Read(br, order, &magic); err != nil {
		return true
	}
	var ifd0Offset uint32
	if err := binary.Read(br, order, &ifd0Offset); err != nil || ifd0Offset < 8 {
		return true
	}
	if _, err := io.CopyN(io.Discard, br, int64(ifd0Offset-8)); err != nil {
		return true
	}
	var numTags uint16
	if err := binary.Read(br, order, &numTags); err != nil {
		return true
	}
	if numTags > 8192 {
		return true
	}
	for range numTags {
		var tag uint16
		if err := binary.Read(br, order, &tag); err != nil {
			return true
		}
		var fieldType uint16
		if err := binary.Read(br, order, &fieldType); err != nil {
			return true
		}
		var count uint32
		if err := binary.Read(br, order, &count); err != nil {
			return true
		}
		var value uint32
		if err := binary.Read(br, order, &value); err != nil {
			return true
		}
		if tag == orientationTag && fieldType == 3 && count == 1 {
			// 0 (absent) and 1 (normal) need no transform; 2..8 do.
			return value <= 1
		}
	}
	return true
}
