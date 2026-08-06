package config

import (
	"encoding/json"
	"os"
	"path/filepath"
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

func TestSubDocument_Inline(t *testing.T) {
	var o Opaque
	if err := json.Unmarshal([]byte(`{"version":"v1","items":["a"]}`), &o); err != nil {
		t.Fatalf("unmarshal opaque: %v", err)
	}
	data, err := (SubDocument{Inline: &o}).Bytes()
	if err != nil {
		t.Fatalf("SubDocument.Bytes error: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("inline document does not parse: %v", err)
	}
	if decoded["version"] != "v1" {
		t.Fatalf("inline document = %v, want version v1", decoded)
	}
}

func TestSubDocument_File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub.json")
	if err := os.WriteFile(path, []byte(`{"version":"v1"}`), 0o600); err != nil {
		t.Fatalf("write subdocument: %v", err)
	}
	data, err := (SubDocument{File: path}).Bytes()
	if err != nil {
		t.Fatalf("SubDocument.Bytes error: %v", err)
	}
	if string(data) != `{"version":"v1"}` {
		t.Fatalf("SubDocument.Bytes = %q, want file contents", data)
	}
}

func TestSubDocument_ExactlyOneForm(t *testing.T) {
	if _, err := (SubDocument{}).Bytes(); err == nil {
		t.Fatal("empty SubDocument accepted")
	}
	var o Opaque
	if err := json.Unmarshal([]byte(`{"a":1}`), &o); err != nil {
		t.Fatalf("unmarshal opaque: %v", err)
	}
	if _, err := (SubDocument{File: "x.json", Inline: &o}).Bytes(); err == nil {
		t.Fatal("SubDocument with both file and inline accepted")
	}
}
