//go:build windows

package windows

// WFP bindings for per-AppContainer network filters. The struct
// layouts and LazyDLL wrappers are a trimmed port of the Tailscale
// wf package (github.com/freinholz/wf, BSD-3-Clause, Copyright
// (c) 2021 The Inet.Af AUTHORS), reduced to the ALE_AUTH_CONNECT
// layer, sublayer management, and block/permit filters needed by the
// sandbox.

import (
	"crypto/rand"
	"fmt"
	"unsafe"

	"github.com/GizClaw/flowcraft/core/errdefs"
	xwin "golang.org/x/sys/windows"
)

// FWP value types (fwptypes.h).
type wfpDataType uint32

const (
	wfpDataTypeUint64     wfpDataType = 4
	wfpDataTypeUint8      wfpDataType = 1
	wfpDataTypeUint16     wfpDataType = 2
	wfpDataTypeSID        wfpDataType = 13
	wfpDataTypeV4AddrMask wfpDataType = 256
	wfpDataTypeV6AddrMask wfpDataType = 257
)

// wfpMatchType is FWP_MATCH_TYPE.
type wfpMatchType uint32

const wfpMatchEqual wfpMatchType = 0

// wfpActionType is FWPM_ACTION_TYPE.
type wfpActionType uint32

const (
	wfpActionBlock  wfpActionType = 0x1001
	wfpActionPermit wfpActionType = 0x1002
)

// The engine copies each filter before returning, so ordinary heap
// structs are fine; no notinheap annotation is required.
type wfpDisplayData struct {
	Name        *uint16
	Description *uint16
}

type wfpSessionFlags uint32

const wfpSessionDynamic wfpSessionFlags = 1

type wfpSession struct {
	SessionKey           xwin.GUID
	DisplayData          wfpDisplayData
	Flags                wfpSessionFlags
	TxnWaitTimeoutMillis uint32
	ProcessID            uint32
	SID                  *xwin.SID
	Username             *uint16
	KernelMode           uint8
}

type wfpByteBlob struct {
	Size uint32
	Data *uint8
}

type wfpSublayer struct {
	SublayerKey  xwin.GUID
	DisplayData  wfpDisplayData
	Flags        uint32
	ProviderKey  *xwin.GUID
	ProviderData wfpByteBlob
	Weight       uint16
}

type wfpValue struct {
	Type  wfpDataType
	Value uintptr
}

type wfpAction struct {
	Type wfpActionType
	GUID xwin.GUID
}

type wfpFilter struct {
	FilterKey           xwin.GUID
	DisplayData         wfpDisplayData
	Flags               uint32
	ProviderKey         *xwin.GUID
	ProviderData        wfpByteBlob
	LayerKey            xwin.GUID
	SublayerKey         xwin.GUID
	Weight              wfpValue
	NumFilterConditions uint32
	FilterConditions    *wfpFilterCondition
	Action              wfpAction
	RawContext          uint64
	ProviderContextKey  xwin.GUID
	Reserved            *xwin.GUID
	FilterID            uint64
	EffectiveWeight     wfpValue
}

type wfpConditionValue struct {
	Type  wfpDataType
	Value uintptr
}

type wfpFilterCondition struct {
	FieldKey  xwin.GUID
	MatchType wfpMatchType
	Value     wfpConditionValue
}

type wfpV4AddrAndMask struct {
	Addr, Mask uint32
}

type wfpV6AddrAndMask struct {
	Addr         [16]byte
	PrefixLength uint8
}

// WFP layer and condition GUIDs (fwpmu.h / fwpmtypes.h).
var (
	fwpmLayerAleAuthConnectV4 = xwin.GUID{
		Data1: 0xc38d57d1, Data2: 0x05a7, Data3: 0x4c33,
		Data4: [8]byte{0x90, 0x4f, 0x7f, 0xbc, 0xee, 0xe6, 0x0e, 0x82},
	}
	fwpmLayerAleAuthConnectV6 = xwin.GUID{
		Data1: 0x4a72393b, Data2: 0x319f, Data3: 0x44bc,
		Data4: [8]byte{0x84, 0xc3, 0xba, 0x54, 0xdc, 0xb3, 0xb6, 0xb4},
	}
	fwpmConditionAlePackageID = xwin.GUID{
		Data1: 0x71bc78fa, Data2: 0xf17c, Data3: 0x4997,
		Data4: [8]byte{0xa6, 0x02, 0x6a, 0xbb, 0x26, 0x1f, 0x35, 0x1c},
	}
	fwpmConditionIPRemoteAddr = xwin.GUID{
		Data1: 0xb235ae9a, Data2: 0x1d64, Data3: 0x49b8,
		Data4: [8]byte{0xa4, 0x4c, 0x5f, 0xf3, 0xd9, 0x09, 0x50, 0x45},
	}
	fwpmConditionIPRemotePort = xwin.GUID{
		Data1: 0xc35a604d, Data2: 0xd22b, Data3: 0x4e1a,
		Data4: [8]byte{0x91, 0xb4, 0x68, 0xf6, 0x74, 0xee, 0x67, 0x4b},
	}
	fwpmConditionIPProtocol = xwin.GUID{
		Data1: 0x3971ef2b, Data2: 0x623e, Data3: 0x4f9a,
		Data4: [8]byte{0x8c, 0xb1, 0x6e, 0x79, 0xb8, 0x06, 0xb9, 0xa7},
	}
)

var (
	modFwpuclnt                  = xwin.NewLazySystemDLL("fwpuclnt.dll")
	procFwpmEngineOpen0          = modFwpuclnt.NewProc("FwpmEngineOpen0")
	procFwpmEngineClose0         = modFwpuclnt.NewProc("FwpmEngineClose0")
	procFwpmSubLayerAdd0         = modFwpuclnt.NewProc("FwpmSubLayerAdd0")
	procFwpmSubLayerDeleteByKey0 = modFwpuclnt.NewProc("FwpmSubLayerDeleteByKey0")
	procFwpmFilterAdd0           = modFwpuclnt.NewProc("FwpmFilterAdd0")
	procFwpmFilterDeleteById0    = modFwpuclnt.NewProc("FwpmFilterDeleteById0")
)

const wfpAuthnServiceWinNT = 0x0a

// wfpIsolation owns one engine session and sublayer; all filters are
// scoped to the sublayer and removed on Close. The session is dynamic,
// so the engine also drops them if the process dies.
type wfpIsolation struct {
	engine    xwin.Handle
	sublayer  xwin.GUID
	filterIDs []uint64
	closed    bool
}

func openWFPSession() (xwin.Handle, error) {
	session := &wfpSession{Flags: wfpSessionDynamic}
	var engine xwin.Handle
	r1, _, e1 := procFwpmEngineOpen0.Call(
		0, // serverName: local
		wfpAuthnServiceWinNT,
		0, // authIdentity
		uintptr(unsafe.Pointer(session)),
		uintptr(unsafe.Pointer(&engine)),
	)
	if r1 != 0 {
		// ERROR_ACCESS_DENIED / ERROR_ACCESS_DENIED variants: opening
		// the BFE engine requires elevation. Fail closed with
		// NotAvailable so callers and probes can distinguish "needs
		// admin" from a real failure.
		if r1 == 5 || r1 == 0x80070005 {
			return 0, errdefs.NotAvailablef(
				"windows: FwpmEngineOpen0: requires an elevated host (0x%x)", r1)
		}
		return 0, errdefs.Internal(fmt.Errorf("windows: FwpmEngineOpen0: 0x%x (%v)", r1, e1))
	}
	return engine, nil
}

func closeWFPSession(engine xwin.Handle) {
	if engine != 0 {
		_, _, _ = procFwpmEngineClose0.Call(uintptr(engine))
	}
}

func wfpAddSublayer(engine xwin.Handle, key xwin.GUID) error {
	name, _ := xwin.UTF16PtrFromString("flowcraft sandbox net policy")
	sl := &wfpSublayer{
		SublayerKey: key,
		DisplayData: wfpDisplayData{Name: name},
		Weight:      0x4000,
	}
	r1, _, e1 := procFwpmSubLayerAdd0.Call(
		uintptr(engine), uintptr(unsafe.Pointer(sl)), 0)
	if r1 != 0 {
		return errdefs.Internal(fmt.Errorf("windows: FwpmSubLayerAdd0: 0x%x (%v)", r1, e1))
	}
	return nil
}

func wfpDeleteSublayer(engine xwin.Handle, key xwin.GUID) {
	_, _, _ = procFwpmSubLayerDeleteByKey0.Call(
		uintptr(engine), uintptr(unsafe.Pointer(&key)))
}

// wfpCondition builds one filter condition value.
type wfpCondition struct {
	field xwin.GUID
	value wfpConditionValue
}

func sidCondition(sid *xwin.SID) wfpCondition {
	return wfpCondition{
		field: fwpmConditionAlePackageID,
		value: wfpConditionValue{Type: wfpDataTypeSID, Value: uintptr(unsafe.Pointer(sid))},
	}
}

func v4AddrCondition(addr uint32) wfpCondition {
	mask := &wfpV4AddrAndMask{Addr: addr, Mask: 0xffffffff}
	return wfpCondition{
		field: fwpmConditionIPRemoteAddr,
		value: wfpConditionValue{Type: wfpDataTypeV4AddrMask, Value: uintptr(unsafe.Pointer(mask))},
	}
}

func v6LoopbackCondition() wfpCondition {
	var mask wfpV6AddrAndMask
	mask.Addr[15] = 1 // ::1
	mask.PrefixLength = 128
	return wfpCondition{
		field: fwpmConditionIPRemoteAddr,
		value: wfpConditionValue{Type: wfpDataTypeV6AddrMask, Value: uintptr(unsafe.Pointer(&mask))},
	}
}

func portCondition(port uint16) wfpCondition {
	return wfpCondition{
		field: fwpmConditionIPRemotePort,
		value: wfpConditionValue{Type: wfpDataTypeUint16, Value: uintptr(port)},
	}
}

func tcpProtocolCondition() wfpCondition {
	return wfpCondition{
		field: fwpmConditionIPProtocol,
		value: wfpConditionValue{Type: wfpDataTypeUint8, Value: 6},
	}
}

// wfpAddFilter adds one filter to the session's sublayer at
// ALE_AUTH_CONNECT and records its ID for cleanup.
func (w *wfpIsolation) wfpAddFilter(layer xwin.GUID, name string, action wfpActionType,
	weight uint64, conds []wfpCondition) error {
	namePtr, _ := xwin.UTF16PtrFromString(name)
	cconds := make([]wfpFilterCondition, len(conds))
	for i, c := range conds {
		cconds[i] = wfpFilterCondition{
			FieldKey:  c.field,
			MatchType: wfpMatchEqual,
			Value:     c.value,
		}
	}
	// FWP_VALUE0 carries FWP_UINT64 as a pointer to the value (not the
	// value itself); the engine dereferences it while copying the
	// filter, so the stack slot stays valid for the call below.
	weightValue := weight
	var condsPtr *wfpFilterCondition
	if len(cconds) > 0 {
		condsPtr = &cconds[0]
	}
	filter := &wfpFilter{
		DisplayData:         wfpDisplayData{Name: namePtr},
		LayerKey:            layer,
		SublayerKey:         w.sublayer,
		Weight:              wfpValue{Type: wfpDataTypeUint64, Value: uintptr(unsafe.Pointer(&weightValue))},
		NumFilterConditions: uint32(len(cconds)),
		FilterConditions:    condsPtr,
		Action:              wfpAction{Type: action},
	}
	var id uint64
	r1, _, e1 := procFwpmFilterAdd0.Call(
		uintptr(w.engine), uintptr(unsafe.Pointer(filter)), 0, uintptr(unsafe.Pointer(&id)))
	if r1 != 0 {
		return errdefs.Internal(fmt.Errorf("windows: FwpmFilterAdd0(%s): 0x%x (%v)", name, r1, e1))
	}
	w.filterIDs = append(w.filterIDs, id)
	return nil
}

// wfpDeleteFilter removes one recorded filter by ID.
func (w *wfpIsolation) wfpDeleteFilter(id uint64) {
	_, _, _ = procFwpmFilterDeleteById0.Call(uintptr(w.engine), uintptr(id))
}

// newWFPIsolation opens a dynamic engine session and creates the
// runner's sublayer.
func newWFPIsolation() (*wfpIsolation, error) {
	engine, err := openWFPSession()
	if err != nil {
		return nil, err
	}
	var sublayer xwin.GUID
	if err := randomGUID(&sublayer); err != nil {
		closeWFPSession(engine)
		return nil, errdefs.Internal(fmt.Errorf("windows: wfp sublayer guid: %w", err))
	}
	if err := wfpAddSublayer(engine, sublayer); err != nil {
		closeWFPSession(engine)
		return nil, err
	}
	return &wfpIsolation{engine: engine, sublayer: sublayer}, nil
}

// Close removes every filter, the sublayer, and the engine session.
func (w *wfpIsolation) Close() {
	if w == nil || w.closed {
		return
	}
	w.closed = true
	for _, id := range w.filterIDs {
		w.wfpDeleteFilter(id)
	}
	w.filterIDs = nil
	wfpDeleteSublayer(w.engine, w.sublayer)
	closeWFPSession(w.engine)
	w.engine = 0
}

// randomGUID fills g with random bytes, setting the RFC 4122 version
// and variant bits so the value is a valid GUID.
func randomGUID(g *xwin.GUID) error {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return err
	}
	raw[7] = (raw[7] & 0x0f) | 0x40 // version 4
	raw[8] = (raw[8] & 0x3f) | 0x80 // RFC 4122 variant
	g.Data1 = uint32(raw[0]) | uint32(raw[1])<<8 | uint32(raw[2])<<16 | uint32(raw[3])<<24
	g.Data2 = uint16(raw[4]) | uint16(raw[5])<<8
	g.Data3 = uint16(raw[6]) | uint16(raw[7])<<8
	copy(g.Data4[:], raw[8:])
	return nil
}
