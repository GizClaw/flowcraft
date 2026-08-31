//go:build windows

package windows

import (
	"testing"
	"unsafe"

	xwin "golang.org/x/sys/windows"
)

func TestWFPConditionConstruction(t *testing.T) {
	// A SID condition carries the SID pointer as the value.
	sid, err := xwin.CreateWellKnownSid(xwin.WinLocalSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid: %v", err)
	}
	c := sidCondition(sid)
	if c.field != fwpmConditionAlePackageID {
		t.Fatalf("sid condition field = %v, want ALE_PACKAGE_ID", c.field)
	}
	if c.value.Type != wfpDataTypeSID {
		t.Fatalf("sid condition type = %v, want FWP_SID", c.value.Type)
	}
	if c.value.Value != uintptr(unsafe.Pointer(sid)) {
		t.Fatal("sid condition value does not point at the sid")
	}

	// The loopback v4 condition must encode 127.0.0.1 with a full mask.
	v4 := v4AddrCondition(0x7f000001)
	if v4.field != fwpmConditionIPRemoteAddr || v4.value.Type != wfpDataTypeV4AddrMask {
		t.Fatalf("v4 condition = %+v", v4)
	}
	mask := (*wfpV4AddrAndMask)(unsafe.Pointer(v4.value.Value)) //nolint:govet // test-only reinterpretation of the opaque value slot
	if mask.Addr != 0x7f000001 || mask.Mask != 0xffffffff {
		t.Fatalf("v4 mask = %+v, want 127.0.0.1/32", mask)
	}

	// The v6 condition must encode ::1/128.
	v6 := v6LoopbackCondition()
	if v6.field != fwpmConditionIPRemoteAddr || v6.value.Type != wfpDataTypeV6AddrMask {
		t.Fatalf("v6 condition = %+v", v6)
	}
	v6mask := (*wfpV6AddrAndMask)(unsafe.Pointer(v6.value.Value)) //nolint:govet // test-only reinterpretation of the opaque value slot
	if v6mask.PrefixLength != 128 || v6mask.Addr[15] != 1 {
		t.Fatalf("v6 mask = %+v, want ::1/128", v6mask)
	}

	// Port and protocol conditions carry the numeric value directly.
	p := portCondition(8080)
	if p.value.Type != wfpDataTypeUint16 || p.value.Value != 8080 {
		t.Fatalf("port condition = %+v", p)
	}
	tp := tcpProtocolCondition()
	if tp.value.Type != wfpDataTypeUint8 || tp.value.Value != 6 {
		t.Fatalf("protocol condition = %+v", tp)
	}
}
