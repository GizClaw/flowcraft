package fs

import (
	"testing"
)

func TestVec_RoundTrip(t *testing.T) {
	cases := [][]float32{
		nil,
		{},
		{0},
		{1.25, -2.5, 0, 3.14159, 1e-7},
	}
	for _, c := range cases {
		buf := encodeVec(c)
		got, err := decodeVec(buf)
		if err != nil {
			t.Fatalf("decode %v: %v", c, err)
		}
		if len(got) != len(c) {
			t.Fatalf("len mismatch: got %d, want %d (%v)", len(got), len(c), c)
		}
		for i := range c {
			if got[i] != c[i] {
				t.Fatalf("vec[%d] = %v, want %v", i, got[i], c[i])
			}
		}
	}
}

func TestVec_DecodeBadMagic(t *testing.T) {
	bad := append([]byte("XXXX"), 0, 0, 0, 0)
	if _, err := decodeVec(bad); err == nil {
		t.Fatalf("expected magic mismatch error")
	}
}

func TestVec_DecodeShortHeader(t *testing.T) {
	if _, err := decodeVec([]byte{1, 2, 3}); err == nil {
		t.Fatalf("expected header-too-short error")
	}
}

func TestVec_DecodeLengthMismatch(t *testing.T) {
	v := encodeVec([]float32{1, 2, 3})
	if _, err := decodeVec(v[:len(v)-1]); err == nil {
		t.Fatalf("expected length mismatch error")
	}
}
