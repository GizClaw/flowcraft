package resource

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestExpandNoOptionsReturnsInput(t *testing.T) {
	out, err := Expand(context.Background(), []byte(`{"a": "${env:X}"}`))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if string(out) != `{"a": "${env:X}"}` {
		t.Fatalf("Expand = %s", out)
	}
}

func TestExpandEnv(t *testing.T) {
	lookup := func(name string) (string, bool) {
		if name == "ROOT" {
			return "/srv/flowcraft", true
		}
		return "", false
	}
	out, err := Expand(context.Background(), []byte(`{
		"root": "${env:ROOT}",
		"nested": {"path": "${env:ROOT}/data"},
		"list": ["${env:ROOT}/a", "plain"]
	}`), WithEnv(lookup))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["root"] != "/srv/flowcraft" {
		t.Fatalf("root = %v", got["root"])
	}
	nested := got["nested"].(map[string]any)
	if nested["path"] != "/srv/flowcraft/data" {
		t.Fatalf("nested path = %v", nested["path"])
	}
	list := got["list"].([]any)
	if list[0] != "/srv/flowcraft/a" || list[1] != "plain" {
		t.Fatalf("list = %v", list)
	}
}

func TestExpandBase(t *testing.T) {
	out, err := Expand(context.Background(), []byte(`{
		"dir": "${base}",
		"file": "${base:tools/tools.yaml}"
	}`), ExpandBase("/tmp/deploy"))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["dir"] != "/tmp/deploy" {
		t.Fatalf("dir = %v", got["dir"])
	}
	if got["file"] != filepath.Join("/tmp/deploy", "tools/tools.yaml") {
		t.Fatalf("file = %v", got["file"])
	}
}

func TestExpandHome(t *testing.T) {
	out, err := Expand(context.Background(), []byte(`{
		"bare": "~",
		"sub": "~/flowcraft",
		"ref": "${home:data}",
		"reffull": "${home}"
	}`), ExpandHome())
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["bare"] != home || got["sub"] != filepath.Join(home, "flowcraft") ||
		got["ref"] != filepath.Join(home, "data") || got["reffull"] != home {
		t.Fatalf("home expansion = %v", got)
	}
}

func TestExpandErrors(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	for _, tc := range []struct {
		name string
		raw  string
		opts []ExpandOption
	}{
		{"missing env", `{"a": "${env:UNSET}"}`, []ExpandOption{WithEnv(lookup)}},
		{"env not enabled", `{"a": "${env:X}"}`, []ExpandOption{ExpandBase("/tmp")}},
		{"base not enabled", `{"a": "${base}"}`, []ExpandOption{ExpandEnv()}},
		{"unknown ref", `{"a": "${foo}"}`, []ExpandOption{ExpandEnv()}},
		{"unterminated", `{"a": "${env:X"}`, []ExpandOption{ExpandEnv()}},
	} {
		if _, err := Expand(context.Background(), []byte(tc.raw), tc.opts...); !errdefs.IsValidation(err) {
			t.Fatalf("%s: error = %v, want validation", tc.name, err)
		}
	}
}

func TestDecodeSettingsWithExpansion(t *testing.T) {
	type settings struct {
		Root string `json:"root"`
	}
	lookup := func(name string) (string, bool) {
		if name == "ROOT" {
			return "/srv/flowcraft", true
		}
		return "", false
	}
	got, err := DecodeTyped[settings](
		context.Background(),
		[]byte(`{"root": "${env:ROOT}/data"}`), WithEnv(lookup))
	if err != nil {
		t.Fatalf("DecodeTyped: %v", err)
	}
	if got.Root != "/srv/flowcraft/data" {
		t.Fatalf("Root = %q", got.Root)
	}
}

func TestExpandEscapedReference(t *testing.T) {
	out, err := Expand(context.Background(), []byte(`{
		"escaped": "\\${env:NOPE}",
		"mixed": "${env:ROOT}-\\${env:NOPE}"
	}`), WithEnv(func(name string) (string, bool) {
		if name == "ROOT" {
			return "/srv", true
		}
		return "", false
	}))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["escaped"] != "${env:NOPE}" {
		t.Fatalf("escaped = %v, want literal ${env:NOPE}", got["escaped"])
	}
	if got["mixed"] != "/srv-${env:NOPE}" {
		t.Fatalf("mixed = %v, want /srv-literal", got["mixed"])
	}
}

func TestExpandEscapedReferenceRequiresClosingBrace(t *testing.T) {
	if _, err := Expand(context.Background(),
		[]byte(`{"a": "\\${env:NOPE"}`), ExpandEnv()); !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want validation for unterminated escaped reference", err)
	}
}

func TestExpandCustomScheme(t *testing.T) {
	resolver := NewResolver(SchemeFunc{
		SchemeName: "cfg",
		Fn: func(_ context.Context, ref string) (any, error) {
			if ref == "x" {
				return "value-x", nil
			}
			return "", errdefs.Validationf("cfg: unknown ref %q", ref)
		},
	})
	out, err := Expand(context.Background(),
		[]byte(`{"root": "${cfg:x}"}`), WithResolver(resolver))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["root"] != "value-x" {
		t.Fatalf("root = %v, want value-x", got["root"])
	}
}

func TestExpandDeferredBoardReferences(t *testing.T) {
	resolver := NewResolver(EnvScheme(func(name string) (string, bool) {
		if name == "ROOT" {
			return "/srv", true
		}
		return "", false
	})).WithDeferred("board")
	expand := func(raw string) string {
		t.Helper()
		out, err := Expand(context.Background(), []byte(raw), WithResolver(resolver))
		if err != nil {
			t.Fatalf("Expand(%s): %v", raw, err)
		}
		return string(out)
	}
	// Plain board refs (with the graph ":default" syntax) pass through
	// untouched for the graph engine.
	if got := expand(`{"a": "${board.user.name}"}`); got != `{"a":"${board.user.name}"}` {
		t.Fatalf("board ref = %s", got)
	}
	if got := expand(`{"a": "${board.limit:3}"}`); got != `{"a":"${board.limit:3}"}` {
		t.Fatalf("board default ref = %s", got)
	}
	// The escaped board form keeps its backslash, so the graph engine's
	// own escape semantics survive.
	if got := expand(`{"a": "\\${board.x}"}`); got != `{"a":"\\${board.x}"}` {
		t.Fatalf("escaped board ref = %s", got)
	}
	// Non-board refs still expand; non-board escapes still become
	// literals.
	if got := expand(`{"a": "${board.user.name}-${env:ROOT}"}`); got != `{"a":"${board.user.name}-/srv"}` {
		t.Fatalf("mixed refs = %s", got)
	}
	if got := expand(`{"a": "\\${env:NOPE}"}`); got != `{"a":"${env:NOPE}"}` {
		t.Fatalf("escaped env ref = %s", got)
	}
}

func TestExpandDisabledSchemeErrors(t *testing.T) {
	if _, err := Expand(context.Background(),
		[]byte(`{"a": "${cfg:x}"}`), ExpandEnv()); !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want validation for disabled scheme", err)
	}
}
