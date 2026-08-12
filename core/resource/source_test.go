package resource

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestParseSourceStringInline(t *testing.T) {
	src, err := ParseSource(json.RawMessage(`"hello"`))
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	if src.IsRef() || string(src.Inline) != "hello" {
		t.Fatalf("source = %+v", src)
	}
}

func TestParseSourceStructuredInline(t *testing.T) {
	raw := json.RawMessage(`{"root": "/tmp", "file": "nested"}`)
	src, err := ParseSource(raw)
	if err != nil {
		t.Fatalf("ParseSource: %v", err)
	}
	if src.IsRef() {
		t.Fatalf("nested file key must stay inline: %+v", src)
	}
}

func TestParseSourceRefs(t *testing.T) {
	file, err := ParseSource(json.RawMessage(`{"file": "tools.yaml"}`))
	if err != nil || file.File != "tools.yaml" {
		t.Fatalf("file source = %+v, err = %v", file, err)
	}
	embed, err := ParseSource(json.RawMessage(`{"embed": "assets/tools.yaml"}`))
	if err != nil || embed.Embed != "assets/tools.yaml" {
		t.Fatalf("embed source = %+v, err = %v", embed, err)
	}
}

func TestParseSourceRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"empty", ""},
		{"empty object", `{}`},
		{"non-string scalar", `42`},
		{"file non-string", `{"file": 1}`},
		{"empty file", `{"file": ""}`},
	} {
		if _, err := ParseSource(json.RawMessage(tc.raw)); !errdefs.IsValidation(err) {
			t.Fatalf("%s: error = %v, want validation", tc.name, err)
		}
	}
}
