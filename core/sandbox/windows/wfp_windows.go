//go:build windows

package windows

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file binds the Windows Filtering Platform (fwpuclnt.dll)
// enough to install persistent per-account filters: a catch-all block
// for the sandbox account's outbound connects and inbound accepts,
// with an explicit loopback allow so the host-side enforcement proxy
// (core/utils/net, bound to 127.0.0.1) stays reachable. Only the
// offline account carries these filters; the online account is
// unconstrained (NetDefault posture).

var (
	modfwpuclnt = windows.NewLazySystemDLL("fwpuclnt.dll")

	procFwpmEngineOpen0        = modfwpuclnt.NewProc("FwpmEngineOpen0")
	procFwpmTransactionBegin0  = modfwpuclnt.NewProc("FwpmTransactionBegin0")
	procFwpmTransactionCommit0 = modfwpuclnt.NewProc("FwpmTransactionCommit0")
	procFwpmTransactionAbort0  = modfwpuclnt.NewProc("FwpmTransactionAbort0")
	procFwpmProviderAdd0       = modfwpuclnt.NewProc("FwpmProviderAdd0")
	procFwpmSubLayerAdd0       = modfwpuclnt.NewProc("FwpmSubLayerAdd0")
	procFwpmFilterAdd0         = modfwpuclnt.NewProc("FwpmFilterAdd0")
	procFwpmFilterDeleteByKey0 = modfwpuclnt.NewProc("FwpmFilterDeleteByKey0")
	procFwpmEngineClose0       = modfwpuclnt.NewProc("FwpmEngineClose0")

	procBuildExplicitAccessWithNameW = modadvapi32.NewProc("BuildExplicitAccessWithNameW")
	procBuildSecurityDescriptorW     = modadvapi32.NewProc("BuildSecurityDescriptorW")
)

// FWP / FWPM constants (windows-sys values).
const (
	fwpActionBlock   = 4097
	fwpActionPermit  = 4098
	fwpMatchEqual    = 0
	fwpEmpty         = 0
	fwpUint16        = 2
	fwpUint32        = 3
	fwpUint64        = 4
	fwpSecDescType   = 14 // FWP_SECURITY_DESCRIPTOR_TYPE
	fwpConditionLoop = 1  // FWP_CONDITION_FLAG_IS_LOOPBACK

	fwpmFilterFlagPersistent   = 1
	fwpmProviderFlagPersistent = 1
	fwpmSubLayerFlagPersistent = 1
	fwpmActrlMatchFilter       = 1

	rpcCAuthnDefault = 0xffffffff

	fwpEAlreadyExists  = 0x80320009
	fwpEFilterNotFound = 0x80320003
	fwpENotFound       = 0x80320008
)

// WFP identities. These are FlowCraft-owned stable GUIDs; do not
// regenerate them or old persistent filters are orphaned.
var (
	wfpProviderGUID = windows.GUID{
		Data1: 0x2e9c1a4b, Data2: 0x6f3d, Data3: 0x4a8b,
		Data4: [8]byte{0x9c, 0x5e, 0x1a, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f},
	}
	wfpSubLayerGUID = windows.GUID{
		Data1: 0x7a1b2c3d, Data2: 0x4e5f, Data3: 0x6071,
		Data4: [8]byte{0x82, 0x93, 0xa4, 0xb5, 0xc6, 0xd7, 0xe8, 0xf9},
	}
)

// wfpLayer is one filter layer plus its allow/block filter keys.
type wfpLayer struct {
	name     string
	layerKey windows.GUID
	allowKey windows.GUID
	blockKey windows.GUID
}

// wfpLayers covers outbound connects and inbound accepts for IPv4 and
// IPv6. Each layer gets one allow-loopback filter and one catch-all
// block filter, both keyed to the sandbox account's SID.
var wfpLayers = []wfpLayer{
	{
		name:     "ale_auth_connect_v4",
		layerKey: guid(0xc38d57d1, 0x05a7, 0x4c33, [8]byte{0x90, 0x4f, 0x7f, 0xbc, 0xee, 0xe6, 0x0e, 0x82}),
		allowKey: guid(0x11111111, 0x1111, 0x1111, [8]byte{0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11, 0x11}),
		blockKey: guid(0x22222222, 0x2222, 0x2222, [8]byte{0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22, 0x22}),
	},
	{
		name:     "ale_auth_connect_v6",
		layerKey: guid(0x4a72393b, 0x319f, 0x44bc, [8]byte{0x84, 0xc3, 0xba, 0x54, 0xdc, 0xb3, 0xb6, 0xb4}),
		allowKey: guid(0x33333333, 0x3333, 0x3333, [8]byte{0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33, 0x33}),
		blockKey: guid(0x44444444, 0x4444, 0x4444, [8]byte{0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44, 0x44}),
	},
	{
		name:     "ale_auth_recv_accept_v4",
		layerKey: guid(0xe1cd9fe7, 0xf4b5, 0x4273, [8]byte{0x96, 0xc0, 0x59, 0x2e, 0x48, 0x7b, 0x86, 0x50}),
		allowKey: guid(0x55555555, 0x5555, 0x5555, [8]byte{0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55, 0x55}),
		blockKey: guid(0x66666666, 0x6666, 0x6666, [8]byte{0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66, 0x66}),
	},
	{
		name:     "ale_auth_recv_accept_v6",
		layerKey: guid(0xa3b42c97, 0x9f04, 0x4672, [8]byte{0xb8, 0x7e, 0xce, 0xe9, 0xc4, 0x83, 0x25, 0x7f}),
		allowKey: guid(0x77777777, 0x7777, 0x7777, [8]byte{0x77, 0x77, 0x77, 0x77, 0x77, 0x77, 0x77, 0x77}),
		blockKey: guid(0x88888888, 0x8888, 0x8888, [8]byte{0x88, 0x88, 0x88, 0x88, 0x88, 0x88, 0x88, 0x88}),
	},
}

// WFP condition GUIDs.
var (
	condUserID = guid(0xaf043a0a, 0xb34d, 0x4f86, [8]byte{0x97, 0x9c, 0xc9, 0x03, 0x71, 0xaf, 0x6e, 0x66})
	condFlags  = guid(0x632ce23b, 0x5167, 0x435c, [8]byte{0x86, 0xd7, 0xe9, 0x03, 0x68, 0x4a, 0xa8, 0x0c})
)

func guid(data1 uint32, data2, data3 uint16, data4 [8]byte) windows.GUID {
	return windows.GUID{Data1: data1, Data2: data2, Data3: data3, Data4: data4}
}

// WFP struct layouts (windows-sys FWPM0_* shapes, x64).
type fwpByteBlob struct {
	size uint32
	data *byte
}

type fwpValue0 struct {
	typ   uint32
	_     uint32
	value uintptr // union: pointer to u64 for weights, value for small types
}

type fwpConditionValue0 struct {
	typ   uint32
	_     uint32
	value uintptr // union: value or pointer (byte blob / SD blob)
}

type fwpmDisplayData0 struct {
	name        *uint16
	description *uint16
}

type fwpmFilterCondition0 struct {
	fieldKey       windows.GUID
	matchType      uint32
	conditionValue fwpConditionValue0
}

type fwpmAction0 struct {
	typ uint32
	_   [16]byte // FWPM_ACTION0_0 union: two GUIDs (filterType / calloutKey)
}

type fwpmFilter0 struct {
	filterKey           windows.GUID
	displayData         fwpmDisplayData0
	flags               uint32
	providerKey         *windows.GUID
	providerData        fwpByteBlob
	layerKey            windows.GUID
	subLayerKey         windows.GUID
	weight              fwpValue0
	numFilterConditions uint32
	filterCondition     *fwpmFilterCondition0
	action              fwpmAction0
	_                   [16]byte // FWPM_FILTER0_0 union
	reserved            *windows.GUID
	filterID            uint64
	effectiveWeight     fwpValue0
}

type fwpmProvider0 struct {
	providerKey  windows.GUID
	displayData  fwpmDisplayData0
	flags        uint32
	providerData fwpByteBlob
	serviceName  *uint16
}

type fwpmSubLayer0 struct {
	subLayerKey  windows.GUID
	displayData  fwpmDisplayData0
	flags        uint32
	providerKey  *windows.GUID
	providerData fwpByteBlob
	weight       uint16
}

type fwpmSession0 struct {
	sessionKey           windows.GUID
	displayData          fwpmDisplayData0
	flags                uint32
	txnWaitTimeoutInMSec uint32
	processID            uint32
	sid                  *windows.SID
	username             *uint16
	kernelMode           uint32
}

// installWFPFilters installs the persistent offline-account filters.
// It must run elevated. Reinstallation is idempotent: existing
// filters are deleted by key before being re-added.
func installWFPFilters(username string) (int, error) {
	engine, err := wfpEngineOpen()
	if err != nil {
		return 0, err
	}
	defer wfpEngineClose(engine)

	user, free, err := userMatchCondition(username)
	if err != nil {
		return 0, err
	}
	defer free()

	if err := wfpTransaction(engine, func() error {
		if err := wfpEnsureProvider(engine); err != nil {
			return err
		}
		if err := wfpEnsureSubLayer(engine); err != nil {
			return err
		}
		for _, layer := range wfpLayers {
			if err := wfpDeleteFilter(engine, layer.allowKey); err != nil {
				return err
			}
			if err := wfpDeleteFilter(engine, layer.blockKey); err != nil {
				return err
			}
			allow := []fwpmFilterCondition0{
				userCondition(user, condUserID),
				{fieldKey: condFlags, matchType: fwpMatchEqual, conditionValue: uint32Value(fwpConditionLoop)},
			}
			if err := wfpAddFilter(engine, layer, layer.allowKey, layer.name+"_allow_loopback", allow, fwpActionPermit, 1<<62); err != nil {
				return err
			}
			block := []fwpmFilterCondition0{userCondition(user, condUserID)}
			if err := wfpAddFilter(engine, layer, layer.blockKey, layer.name+"_block_all", block, fwpActionBlock, 1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return 0, err
	}
	return len(wfpLayers) * 2, nil
}

func wfpEngineOpen() (uintptr, error) {
	session := fwpmSession0{
		txnWaitTimeoutInMSec: windows.INFINITE,
		displayData: fwpmDisplayData0{
			name:        strPtr("flowcraft windows sandbox wfp"),
			description: strPtr("flowcraft windows sandbox wfp session"),
		},
	}
	var engine uintptr
	r, _, _ := procFwpmEngineOpen0.Call(
		0,
		rpcCAuthnDefault,
		0,
		uintptr(unsafe.Pointer(&session)),
		uintptr(unsafe.Pointer(&engine)),
	)
	if r != 0 {
		return 0, fmt.Errorf("windows/wfp: FwpmEngineOpen0: 0x%x", r)
	}
	return engine, nil
}

func wfpEngineClose(engine uintptr) {
	_, _, _ = procFwpmEngineClose0.Call(engine)
}

func wfpTransaction(engine uintptr, fn func() error) error {
	if r, _, _ := procFwpmTransactionBegin0.Call(engine, 0); r != 0 {
		return fmt.Errorf("windows/wfp: FwpmTransactionBegin0: 0x%x", r)
	}
	if err := fn(); err != nil {
		_, _, _ = procFwpmTransactionAbort0.Call(engine)
		return err
	}
	if r, _, _ := procFwpmTransactionCommit0.Call(engine); r != 0 {
		_, _, _ = procFwpmTransactionAbort0.Call(engine)
		return fmt.Errorf("windows/wfp: FwpmTransactionCommit0: 0x%x", r)
	}
	return nil
}

func wfpEnsureProvider(engine uintptr) error {
	provider := fwpmProvider0{
		providerKey: wfpProviderGUID,
		displayData: fwpmDisplayData0{
			name:        strPtr("FlowCraft Windows Sandbox WFP"),
			description: strPtr("Persistent provider for FlowCraft sandbox filters"),
		},
		flags: fwpmProviderFlagPersistent,
	}
	r, _, _ := procFwpmProviderAdd0.Call(engine, uintptr(unsafe.Pointer(&provider)), 0)
	if r != 0 && r != fwpEAlreadyExists {
		return fmt.Errorf("windows/wfp: FwpmProviderAdd0: 0x%x", r)
	}
	return nil
}

func wfpEnsureSubLayer(engine uintptr) error {
	sublayer := fwpmSubLayer0{
		subLayerKey: wfpSubLayerGUID,
		displayData: fwpmDisplayData0{
			name:        strPtr("FlowCraft Windows Sandbox WFP"),
			description: strPtr("Persistent sublayer for FlowCraft sandbox filters"),
		},
		flags:       fwpmSubLayerFlagPersistent,
		providerKey: &wfpProviderGUID,
		weight:      0xffff,
	}
	r, _, _ := procFwpmSubLayerAdd0.Call(engine, uintptr(unsafe.Pointer(&sublayer)), 0)
	if r != 0 && r != fwpEAlreadyExists {
		return fmt.Errorf("windows/wfp: FwpmSubLayerAdd0: 0x%x", r)
	}
	return nil
}

func wfpDeleteFilter(engine uintptr, key windows.GUID) error {
	r, _, _ := procFwpmFilterDeleteByKey0.Call(engine, uintptr(unsafe.Pointer(&key)))
	if r != 0 && r != fwpEFilterNotFound && r != fwpENotFound {
		return fmt.Errorf("windows/wfp: FwpmFilterDeleteByKey0: 0x%x", r)
	}
	return nil
}

func wfpAddFilter(engine uintptr, layer wfpLayer, key windows.GUID, name string, conditions []fwpmFilterCondition0, action uint32, weight uint64) error {
	var condPtr *fwpmFilterCondition0
	if len(conditions) > 0 {
		condPtr = &conditions[0]
	}
	weightVal := weight
	filter := fwpmFilter0{
		filterKey: key,
		displayData: fwpmDisplayData0{
			name:        strPtr(name),
			description: strPtr("FlowCraft sandbox " + name),
		},
		flags:               fwpmFilterFlagPersistent,
		providerKey:         &wfpProviderGUID,
		layerKey:            layer.layerKey,
		subLayerKey:         wfpSubLayerGUID,
		weight:              fwpValue0{typ: fwpUint64, value: uintptr(unsafe.Pointer(&weightVal))},
		numFilterConditions: uint32(len(conditions)),
		filterCondition:     condPtr,
		action:              fwpmAction0{typ: action},
	}
	var id uint64
	r, _, _ := procFwpmFilterAdd0.Call(engine, uintptr(unsafe.Pointer(&filter)), 0, uintptr(unsafe.Pointer(&id)))
	if r != 0 {
		return fmt.Errorf("windows/wfp: FwpmFilterAdd0(%s): 0x%x", name, r)
	}
	return nil
}

func userCondition(blob fwpByteBlob, key windows.GUID) fwpmFilterCondition0 {
	return fwpmFilterCondition0{
		fieldKey:  key,
		matchType: fwpMatchEqual,
		conditionValue: fwpConditionValue0{
			typ:   fwpSecDescType,
			value: uintptr(unsafe.Pointer(&blob)),
		},
	}
}

func uint32Value(v uint32) fwpConditionValue0 {
	return fwpConditionValue0{typ: fwpUint32, value: uintptr(v)}
}

// userMatchCondition builds the FWP_CONDITION_ALE_USER_ID value: a
// security descriptor granting FWP_ACTRL_MATCH_FILTER to username,
// exactly like codex-rs's UserMatchCondition.
func userMatchCondition(username string) (fwpByteBlob, func(), error) {
	name, err := windows.UTF16PtrFromString(username)
	if err != nil {
		return fwpByteBlob{}, nil, err
	}
	var access windows.EXPLICIT_ACCESS
	r1, _, _ := procBuildExplicitAccessWithNameW.Call(
		uintptr(unsafe.Pointer(&access)),
		uintptr(unsafe.Pointer(name)),
		fwpmActrlMatchFilter,
		uintptr(windows.GRANT_ACCESS),
		0,
	)
	if r1 != 0 {
		return fwpByteBlob{}, nil, fmt.Errorf("windows/wfp: BuildExplicitAccessWithNameW: 0x%x", r1)
	}
	var sd uintptr
	var size uint32
	r2, _, _ := procBuildSecurityDescriptorW.Call(
		0,
		0,
		1,
		uintptr(unsafe.Pointer(&access)),
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&size)),
		uintptr(unsafe.Pointer(&sd)),
	)
	if r2 != 0 {
		return fwpByteBlob{}, nil, fmt.Errorf("windows/wfp: BuildSecurityDescriptorW: 0x%x", r2)
	}
	// sd is a LocalAlloc'd heap pointer returned by the API; reinterpret
	// its storage so vet's unsafeptr check stays quiet.
	sdPtr := *(*unsafe.Pointer)(unsafe.Pointer(&sd))
	blob := fwpByteBlob{size: size, data: (*byte)(sdPtr)}
	return blob, func() {
		if sd != 0 {
			_, _ = windows.LocalFree(windows.Handle(sd))
		}
	}, nil
}

func strPtr(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		panic(err)
	}
	return p
}
