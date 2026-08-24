//go:build windows

package windows

import (
	"testing"
	"unsafe"
)

// TestWFPStructSizes locks the WFP struct layouts to the sizes
// reported by windows-sys 0.61.2 (verified against the crate with a
// rustc sizeof probe). A mismatch here means a field offset drifted
// and the ALE filters would silently filter on garbage.
func TestWFPStructSizes(t *testing.T) {
	tests := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"fwpByteBlob", unsafe.Sizeof(fwpByteBlob{}), 16},
		{"fwpValue0", unsafe.Sizeof(fwpValue0{}), 16},
		{"fwpConditionValue0", unsafe.Sizeof(fwpConditionValue0{}), 16},
		{"fwpmDisplayData0", unsafe.Sizeof(fwpmDisplayData0{}), 16},
		{"fwpmFilterCondition0", unsafe.Sizeof(fwpmFilterCondition0{}), 40},
		{"fwpmAction0", unsafe.Sizeof(fwpmAction0{}), 20},
		{"fwpmFilter0", unsafe.Sizeof(fwpmFilter0{}), 200},
		{"fwpmProvider0", unsafe.Sizeof(fwpmProvider0{}), 64},
		{"fwpmSubLayer0", unsafe.Sizeof(fwpmSubLayer0{}), 72},
		{"fwpmSession0", unsafe.Sizeof(fwpmSession0{}), 72},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s size = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}
