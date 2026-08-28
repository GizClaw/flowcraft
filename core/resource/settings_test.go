package resource

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestDecodeSettingsStrict(t *testing.T) {
	type settings struct {
		Path string `json:"path"`
	}
	got, err := DecodeTyped[settings](context.Background(), []byte(`{"path": "/tmp/x"}`))
	if err != nil {
		t.Fatalf("DecodeTyped: %v", err)
	}
	if got.Path != "/tmp/x" {
		t.Fatalf("Path = %q", got.Path)
	}
	if _, err := DecodeTyped[settings](context.Background(), []byte(`{"path": "x", "bogus": 1}`)); !errdefs.IsValidation(err) {
		t.Fatalf("unknown field error = %v, want validation", err)
	}
}

func TestDecodeSettingsEmpty(t *testing.T) {
	type settings struct {
		Path string `json:"path"`
	}
	got, err := DecodeTyped[settings](context.Background(), nil)
	if err != nil {
		t.Fatalf("DecodeTyped(nil): %v", err)
	}
	if got.Path != "" {
		t.Fatalf("Path = %q, want empty", got.Path)
	}
}

func TestOpaqueRoundTrip(t *testing.T) {
	var o Opaque
	if err := o.UnmarshalJSON([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if string(o.Bytes()) != `{"a":1}` {
		t.Fatalf("Bytes = %s", o.Bytes())
	}
	var decoded struct {
		A int `json:"a"`
	}
	if err := o.Decode(context.Background(), &decoded); err != nil || decoded.A != 1 {
		t.Fatalf("Decode = (%v, %+v)", err, decoded)
	}
}
