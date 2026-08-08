package config

import (
	"encoding/json"
	"strings"
	"testing"
)

type testSettings struct {
	Window int `json:"window"`
}

func TestDecodeSettings_Empty(t *testing.T) {
	v, err := DecodeSettings[testSettings](nil)
	if err != nil {
		t.Fatalf("DecodeSettings(nil) error: %v", err)
	}
	if v.Window != 0 {
		t.Fatalf("DecodeSettings(nil) = %+v, want zero value", v)
	}
}

func TestDecodeSettings_StrictUnknownField(t *testing.T) {
	if _, err := DecodeSettings[testSettings](json.RawMessage(`{"window":20,"unknown":1}`)); err == nil {
		t.Fatal("DecodeSettings accepted an unknown field")
	} else if !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("DecodeSettings error = %v, want unknown-field error", err)
	}
}

func TestDecodeSettings_Valid(t *testing.T) {
	v, err := DecodeSettings[testSettings](json.RawMessage(`{"window":20}`))
	if err != nil {
		t.Fatalf("DecodeSettings error: %v", err)
	}
	if v.Window != 20 {
		t.Fatalf("DecodeSettings = %+v, want window 20", v)
	}
}

func TestOpaque_BytesRoundTrip(t *testing.T) {
	var o Opaque
	if err := json.Unmarshal([]byte(`{"a":1,"b":["x","y"]}`), &o); err != nil {
		t.Fatalf("unmarshal opaque: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(o.Bytes(), &decoded); err != nil {
		t.Fatalf("decoded opaque bytes: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("decoded = %v, want two keys", decoded)
	}
}

func TestOpaque_Decode(t *testing.T) {
	var o Opaque
	if err := json.Unmarshal([]byte(`{"window":20}`), &o); err != nil {
		t.Fatalf("unmarshal opaque: %v", err)
	}
	var v testSettings
	if err := o.Decode(&v); err != nil {
		t.Fatalf("Opaque.Decode error: %v", err)
	}
	if v.Window != 20 {
		t.Fatalf("Opaque.Decode = %+v, want window 20", v)
	}
	var nilOpaque *Opaque
	var zero testSettings
	if err := nilOpaque.Decode(&zero); err != nil || zero.Window != 0 {
		t.Fatalf("nil Opaque.Decode = (%+v, %v), want zero value", zero, err)
	}
}
