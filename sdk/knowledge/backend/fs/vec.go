package fs

import (
	"encoding/binary"
	"fmt"
	"math"
)

// vecSuffix is appended to a layer text path to form its embedding sidecar.
const vecSuffix = ".vec"

// vecMagic prefixes every encoded vector. Bumped only on layout changes.
//
// Layout (all little-endian):
//
//	bytes 0..3   magic = "KVEC"
//	bytes 4..7   uint32 dimension count (n)
//	bytes 8..   n * float32 components
//
// 4-byte length implies an upper bound of 2^32 dimensions, which is
// generous given practical embedders top out near 4k.
var vecMagic = [4]byte{'K', 'V', 'E', 'C'}

// vecHeaderSize is the magic + dimension prefix length.
const vecHeaderSize = 8

// encodeVec serialises a float32 slice into the on-disk format. Returns
// nil for an empty vector so the caller can branch on len() without
// allocating.
func encodeVec(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	buf := make([]byte, vecHeaderSize+4*len(v))
	copy(buf[0:4], vecMagic[:])
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(v)))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[vecHeaderSize+4*i:], math.Float32bits(f))
	}
	return buf
}

// decodeVec parses an encoded vector. Returns (nil, nil) for empty
// input so callers can treat "missing sidecar" and "explicitly empty"
// the same way.
func decodeVec(buf []byte) ([]float32, error) {
	if len(buf) == 0 {
		return nil, nil
	}
	if len(buf) < vecHeaderSize {
		return nil, fmt.Errorf("knowledge/fs: vec payload too short (%d bytes)", len(buf))
	}
	if [4]byte{buf[0], buf[1], buf[2], buf[3]} != vecMagic {
		return nil, fmt.Errorf("knowledge/fs: vec magic mismatch")
	}
	n := binary.LittleEndian.Uint32(buf[4:8])
	expected := vecHeaderSize + 4*int(n)
	if len(buf) != expected {
		return nil, fmt.Errorf("knowledge/fs: vec length mismatch (header=%d, got %d bytes)", expected, len(buf))
	}
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[vecHeaderSize+4*i:]))
	}
	return out, nil
}
