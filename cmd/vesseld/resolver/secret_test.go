package resolver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
)

// TestResolveValueRef_Env verifies env-backed secrets resolve when
// AllowSecret is true, fail with a Validation error when the env
// var is unset, and silently return "" in validate-only mode (so
// `vesseld validate` passes on a CI box without prod credentials).
func TestResolveValueRef_Env(t *testing.T) {
	const key = "VESSELD_TEST_RESOLVE_ENV"
	const want = "sk-env-value-deadbeef"
	t.Setenv(key, want)

	ref := v1alpha1.ValueRef{ValueFrom: &v1alpha1.ValueSource{Env: key}}

	t.Run("AllowSecret=true returns value", func(t *testing.T) {
		got, err := resolveValueRef(ref, nil, ResolveOptions{AllowSecret: true}, "test.apiKey")
		if err != nil {
			t.Fatalf("resolveValueRef: %v", err)
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("validate-only skips lookup", func(t *testing.T) {
		got, err := resolveValueRef(ref, nil, ResolveOptions{}, "test.apiKey")
		if err != nil {
			t.Fatalf("resolveValueRef: %v", err)
		}
		if got != "" {
			t.Fatalf("validate-only mode should return empty, got %q", got)
		}
	})

	t.Run("missing env var yields validation error", func(t *testing.T) {
		miss := v1alpha1.ValueRef{ValueFrom: &v1alpha1.ValueSource{Env: "VESSELD_TEST_RESOLVE_ENV_MISSING"}}
		_, err := resolveValueRef(miss, nil, ResolveOptions{AllowSecret: true}, "test.apiKey")
		if err == nil {
			t.Fatal("missing env var: want error, got nil")
		}
		if !strings.Contains(err.Error(), "is not set") {
			t.Fatalf("error = %v, want substring 'is not set'", err)
		}
	})
}

// TestResolveValueRef_File verifies file-backed secrets read the
// file contents (with trailing newline trimmed) and respect
// AllowFile.
func TestResolveValueRef_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(path, []byte("sk-file-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := v1alpha1.ValueRef{ValueFrom: &v1alpha1.ValueSource{File: path}}

	t.Run("AllowFile=true reads + trims", func(t *testing.T) {
		got, err := resolveValueRef(ref, nil, ResolveOptions{AllowFile: true}, "test.apiKey")
		if err != nil {
			t.Fatalf("resolveValueRef: %v", err)
		}
		if got != "sk-file-value" {
			t.Fatalf("got %q, want %q", got, "sk-file-value")
		}
	})

	t.Run("validate-only skips read", func(t *testing.T) {
		got, err := resolveValueRef(ref, nil, ResolveOptions{}, "test.apiKey")
		if err != nil {
			t.Fatalf("validate-only resolveValueRef: %v", err)
		}
		if got != "" {
			t.Fatalf("validate-only mode should return empty, got %q", got)
		}
	})

	t.Run("missing file errors", func(t *testing.T) {
		miss := v1alpha1.ValueRef{ValueFrom: &v1alpha1.ValueSource{File: filepath.Join(dir, "nope.txt")}}
		_, err := resolveValueRef(miss, nil, ResolveOptions{AllowFile: true}, "test.apiKey")
		if err == nil {
			t.Fatal("missing file: want error, got nil")
		}
	})
}

// TestResolveValueRef_SecretRef verifies secretRef.{name,key}
// looks up via the SecretLookup, that AllowSecret=false in
// validate-only mode skips the lookup, and that an unknown name/
// key yields a NotFound-class error.
func TestResolveValueRef_SecretRef(t *testing.T) {
	idx, err := newSecretIndex([]v1alpha1.Secret{{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "openai"},
		Spec:       v1alpha1.SecretSpec{StringData: map[string]string{"apiKey": "sk-from-secret"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ref := v1alpha1.ValueRef{ValueFrom: &v1alpha1.ValueSource{
		SecretRef: &v1alpha1.SecretReference{Name: "openai", Key: "apiKey"},
	}}

	t.Run("AllowSecret=true returns value", func(t *testing.T) {
		got, err := resolveValueRef(ref, idx, ResolveOptions{AllowSecret: true}, "test.apiKey")
		if err != nil {
			t.Fatalf("resolveValueRef: %v", err)
		}
		if got != "sk-from-secret" {
			t.Fatalf("got %q, want %q", got, "sk-from-secret")
		}
	})

	t.Run("validate-only skips lookup", func(t *testing.T) {
		got, err := resolveValueRef(ref, idx, ResolveOptions{}, "test.apiKey")
		if err != nil {
			t.Fatalf("validate-only resolveValueRef: %v", err)
		}
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("unknown secret name", func(t *testing.T) {
		bad := v1alpha1.ValueRef{ValueFrom: &v1alpha1.ValueSource{
			SecretRef: &v1alpha1.SecretReference{Name: "missing", Key: "apiKey"},
		}}
		_, err := resolveValueRef(bad, idx, ResolveOptions{AllowSecret: true}, "test.apiKey")
		if err == nil {
			t.Fatal("unknown secret: want error, got nil")
		}
	})

	t.Run("unknown key in known secret", func(t *testing.T) {
		bad := v1alpha1.ValueRef{ValueFrom: &v1alpha1.ValueSource{
			SecretRef: &v1alpha1.SecretReference{Name: "openai", Key: "missing"},
		}}
		_, err := resolveValueRef(bad, idx, ResolveOptions{AllowSecret: true}, "test.apiKey")
		if err == nil {
			t.Fatal("unknown key: want error, got nil")
		}
	})
}

// TestResolveValueRef_RejectsInlineEmpty asserts an empty ValueRef
// (the YAML shape someone tries when fishing for an "inline plain
// text" path) is rejected at Validate time with a clear message.
// This mirrors the v1alpha1 unit test but pins the resolver-side
// behaviour: callers MUST hit the validation gate, not the source
// switch.
func TestResolveValueRef_RejectsInlineEmpty(t *testing.T) {
	cases := []struct {
		name string
		ref  v1alpha1.ValueRef
	}{
		{"nil-valueFrom", v1alpha1.ValueRef{}},
		{"empty-valueFrom", v1alpha1.ValueRef{ValueFrom: &v1alpha1.ValueSource{}}},
		{"two-sources", v1alpha1.ValueRef{ValueFrom: &v1alpha1.ValueSource{Env: "X", File: "/tmp/y"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveValueRef(tc.ref, nil, ResolveOptions{AllowSecret: true, AllowFile: true}, "test.apiKey")
			if err == nil {
				t.Fatal("want validation error, got nil")
			}
		})
	}
}
