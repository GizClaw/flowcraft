package resource

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func TestRefSplit(t *testing.T) {
	for _, tc := range []struct {
		ref       Ref
		res, item string
		ok        bool
	}{
		{"workspaces", "workspaces", "", true},
		{"workspaces/fs", "workspaces", "fs", true},
		{"", "", "", false},
		{"/leading", "", "", false},
		{"trailing/", "", "", false},
		{"a/b/c", "", "", false},
	} {
		res, item, ok := tc.ref.Split()
		if res != tc.res || item != tc.item || ok != tc.ok {
			t.Fatalf("Split(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.ref, res, item, ok, tc.res, tc.item, tc.ok)
		}
	}
}

func TestRefValidate(t *testing.T) {
	if err := Ref("workspaces/fs").Validate(); err != nil {
		t.Fatalf("valid ref rejected: %v", err)
	}
	for _, bad := range []Ref{"", "a/b/c", "/x", "x/"} {
		if err := bad.Validate(); !errdefs.IsValidation(err) {
			t.Fatalf("Validate(%q) error = %v, want validation", bad, err)
		}
	}
}

func TestDepsValidate(t *testing.T) {
	deps := Deps{"sandbox": "boxes/coding", "workspace": "fs"}
	if err := deps.Validate(); err != nil {
		t.Fatalf("valid deps rejected: %v", err)
	}
	if err := (Deps{"": "fs"}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("empty dep name error = %v, want validation", err)
	}
	if err := (Deps{"x": "a/b/c"}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("bad dep ref error = %v, want validation", err)
	}
}

func TestResourceValidate(t *testing.T) {
	res := Resource{
		Kind:     "sandbox.Registry",
		Impl:     "local",
		Deps:     Deps{"workspace": "fs"},
		Settings: json.RawMessage(`{"root": "/tmp/x"}`),
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("valid resource rejected: %v", err)
	}
	if err := (Resource{}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("empty kind error = %v, want validation", err)
	}
	bad := res
	bad.Settings = json.RawMessage(`{`)
	if err := bad.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("bad settings error = %v, want validation", err)
	}
}
