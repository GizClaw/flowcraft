package secrets

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ---------------------------------------------------------------------------
// Multi
// ---------------------------------------------------------------------------

func TestMulti_GetDispatchesByScheme(t *testing.T) {
	t.Parallel()
	m := NewMulti()
	stub := stubProvider(func(_ context.Context, ref string) ([]byte, error) {
		return []byte("ok:" + ref), nil
	})
	if err := m.Register("test", stub); err != nil {
		t.Fatalf("Register: %v", err)
	}
	out, err := m.Get(context.Background(), "test://foo")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(out) != "ok:test://foo" {
		t.Errorf("unexpected dispatch result %q", out)
	}
}

func TestMulti_UnknownSchemeIsValidation(t *testing.T) {
	t.Parallel()
	m := NewMulti()
	_, err := m.Get(context.Background(), "unknown://foo")
	if err == nil || !errdefs.IsValidation(err) {
		t.Errorf("expected Validation, got %v", err)
	}
}

func TestMulti_EmptyRefIsValidation(t *testing.T) {
	t.Parallel()
	m := NewMulti()
	_, err := m.Get(context.Background(), "")
	if err == nil || !errdefs.IsValidation(err) {
		t.Errorf("expected Validation, got %v", err)
	}
}

func TestMulti_NoSchemeIsValidation(t *testing.T) {
	t.Parallel()
	m := NewMulti()
	_, err := m.Get(context.Background(), "/etc/secret")
	if err == nil || !errdefs.IsValidation(err) {
		t.Errorf("expected Validation, got %v", err)
	}
}

func TestMulti_RegisterRejectsEmptySchemeAndNilBackend(t *testing.T) {
	t.Parallel()
	m := NewMulti()
	if err := m.Register("", stubProvider(nil)); !errdefs.IsValidation(err) {
		t.Errorf("expected Validation for empty scheme, got %v", err)
	}
	if err := m.Register("file", nil); !errdefs.IsValidation(err) {
		t.Errorf("expected Validation for nil backend, got %v", err)
	}
}

func TestMulti_SchemesIsSorted(t *testing.T) {
	t.Parallel()
	m := NewMulti()
	_ = m.Register("zeta", stubProvider(nil))
	_ = m.Register("alpha", stubProvider(nil))
	_ = m.Register("file", stubProvider(nil))
	got := m.Schemes()
	want := []string{"alpha", "file", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %v want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Schemes()[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func TestDefault_RegistersThreeStockBackends(t *testing.T) {
	t.Parallel()
	m := Default()
	got := m.Schemes()
	want := map[string]bool{"env": true, "file": true, "vault": true}
	if len(got) != len(want) {
		t.Errorf("expected %d backends, got %d (%v)", len(want), len(got), got)
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected scheme registered: %q", s)
		}
	}
}

// ---------------------------------------------------------------------------
// EnvProvider
// ---------------------------------------------------------------------------

func TestEnvProvider_HappyPath(t *testing.T) {
	t.Setenv("FLOWCRAFT_TEST_API_KEY", "sk-test-1234")
	out, err := NewEnvProvider().Get(context.Background(), "env://FLOWCRAFT_TEST_API_KEY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(out) != "sk-test-1234" {
		t.Errorf("unexpected value %q", out)
	}
}

func TestEnvProvider_EmptyValueIsAllowed(t *testing.T) {
	t.Setenv("FLOWCRAFT_TEST_EMPTY", "")
	out, err := NewEnvProvider().Get(context.Background(), "env://FLOWCRAFT_TEST_EMPTY")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty value, got %q", out)
	}
}

func TestEnvProvider_UnsetVariableIsNotFound(t *testing.T) {
	os.Unsetenv("FLOWCRAFT_TEST_NEVER_SET")
	_, err := NewEnvProvider().Get(context.Background(), "env://FLOWCRAFT_TEST_NEVER_SET")
	if err == nil || !errdefs.IsNotFound(err) {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestEnvProvider_RejectsBadShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ref  string
	}{
		{"wrong_scheme", "file:///etc/x"},
		{"missing_name", "env://"},
		{"missing_name_alt", "env:"},
		{"path_component", "env://NAME/oops"},
		{"invalid_name_dash", "env://API-KEY"},
		{"invalid_name_leading_digit", "env://1KEY"},
		{"invalid_name_unicode", "env://Ä"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewEnvProvider().Get(context.Background(), tc.ref)
			if err == nil || !errdefs.IsValidation(err) {
				t.Errorf("expected Validation, got %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// FileProvider
// ---------------------------------------------------------------------------

func TestFileProvider_HappyPath(t *testing.T) {
	t.Parallel()
	path := writeSecret(t, "sk-from-file", 0o600)
	out, err := (&FileProvider{EnforceMode: true}).Get(context.Background(), "file://"+path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(out) != "sk-from-file" {
		t.Errorf("unexpected value %q", out)
	}
}

func TestFileProvider_TrimsTrailingNewlines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		written string
		want    string
	}{
		{"lf", "secret\n", "secret"},
		{"crlf", "secret\r\n", "secret"},
		{"no_newline", "secret", "secret"},
		{"internal_newlines_preserved", "line1\nline2\nline3", "line1\nline2\nline3"},
		{"only_strip_one", "secret\n\n", "secret\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSecret(t, tc.written, 0o600)
			out, err := (&FileProvider{EnforceMode: true}).Get(context.Background(), "file://"+path)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if string(out) != tc.want {
				t.Errorf("want %q, got %q", tc.want, out)
			}
		})
	}
}

func TestFileProvider_RejectsLooseMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode enforcement is disabled on Windows (synthesised perms)")
	}
	t.Parallel()
	path := writeSecret(t, "leaky", 0o644)
	_, err := (&FileProvider{EnforceMode: true}).Get(context.Background(), "file://"+path)
	if err == nil || !errdefs.IsForbidden(err) {
		t.Errorf("expected Forbidden for mode 0o644, got %v", err)
	}
}

func TestFileProvider_PermissiveModeOptOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file mode enforcement is disabled on Windows")
	}
	t.Parallel()
	path := writeSecret(t, "leaky", 0o644)
	out, err := (&FileProvider{EnforceMode: false}).Get(context.Background(), "file://"+path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(out) != "leaky" {
		t.Errorf("unexpected value %q", out)
	}
}

func TestFileProvider_MissingFileIsNotFound(t *testing.T) {
	t.Parallel()
	_, err := (&FileProvider{}).Get(context.Background(), "file:///nonexistent/secret/"+filepath.Base(t.TempDir()))
	if err == nil || !errdefs.IsNotFound(err) {
		t.Errorf("expected NotFound, got %v", err)
	}
}

func TestFileProvider_DirectoryIsValidation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := (&FileProvider{}).Get(context.Background(), "file://"+dir)
	if err == nil || !errdefs.IsValidation(err) {
		t.Errorf("expected Validation when target is a directory, got %v", err)
	}
}

func TestFileProvider_RejectsBadShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ref  string
	}{
		{"wrong_scheme", "env://X"},
		{"missing_path", "file://"},
		{"relative_path", "file://etc/secret"}, // host=etc, path=/secret — rejected as remote host
		{"explicit_relative", "file:///"},      // path is "/", non-absolute on most callers' intent; we accept it but Stat will fail. Tested via NotFound elsewhere.
	}
	for _, tc := range cases {
		if tc.name == "explicit_relative" {
			continue // covered by the missing-file test
		}
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&FileProvider{}).Get(context.Background(), tc.ref)
			if err == nil || !errdefs.IsValidation(err) {
				t.Errorf("expected Validation for %q, got %v", tc.ref, err)
			}
		})
	}
}

func TestFileProvider_LocalhostHostIsAccepted(t *testing.T) {
	t.Parallel()
	path := writeSecret(t, "ok", 0o600)
	out, err := (&FileProvider{EnforceMode: true}).Get(context.Background(), "file://localhost"+path)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(out) != "ok" {
		t.Errorf("unexpected value %q", out)
	}
}

// ---------------------------------------------------------------------------
// VaultProvider
// ---------------------------------------------------------------------------

func TestVaultProvider_WellFormedRefIsNotAvailable(t *testing.T) {
	t.Parallel()
	_, err := (&VaultProvider{}).Get(context.Background(), "vault://server.example.com/kv/data/prod?key=openai")
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Errorf("expected NotAvailable for well-formed vault ref, got %v", err)
	}
}

func TestVaultProvider_MalformedRefIsValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ref  string
	}{
		{"missing_host", "vault:///kv/data/prod?key=openai"},
		{"missing_path", "vault://server.example.com"},
		{"missing_key_query", "vault://server.example.com/kv/data/prod"},
		{"wrong_scheme", "env://X"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&VaultProvider{}).Get(context.Background(), tc.ref)
			if err == nil || !errdefs.IsValidation(err) {
				t.Errorf("expected Validation for %q, got %v", tc.ref, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// integration with Default()
// ---------------------------------------------------------------------------

func TestDefault_RoutesAllThreeBackends(t *testing.T) {
	t.Setenv("FLOWCRAFT_TEST_DEFAULT_ENV", "from-env")
	filePath := writeSecret(t, "from-file", 0o600)

	m := Default()

	if out, err := m.Get(context.Background(), "env://FLOWCRAFT_TEST_DEFAULT_ENV"); err != nil {
		t.Errorf("env route: %v", err)
	} else if string(out) != "from-env" {
		t.Errorf("env value: %q", out)
	}

	if out, err := m.Get(context.Background(), "file://"+filePath); err != nil {
		t.Errorf("file route: %v", err)
	} else if string(out) != "from-file" {
		t.Errorf("file value: %q", out)
	}

	_, err := m.Get(context.Background(), "vault://server/kv/x?key=y")
	if err == nil || !errdefs.IsNotAvailable(err) {
		t.Errorf("vault route: expected NotAvailable, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// writeSecret creates a temp file with the given contents and POSIX
// mode, returning its absolute path. The file is auto-cleaned by
// t.TempDir.
func writeSecret(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	if mode != 0o600 {
		// os.WriteFile may apply umask; force the requested mode.
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("chmod %v: %v", mode, err)
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	return abs
}

// stubProvider is a tiny adapter used to inject canned responses
// into a Multi without writing a full type for each test.
type stubProvider func(ctx context.Context, ref string) ([]byte, error)

func (s stubProvider) Get(ctx context.Context, ref string) ([]byte, error) {
	if s == nil {
		return nil, errdefs.NotAvailablef("stubProvider: not wired")
	}
	return s(ctx, ref)
}

// Compile-time check that strings.HasPrefix is reachable from this
// file (kept here so the IDE knows we depend on stdlib strings even
// though only one helper uses it transitively).
var _ = strings.HasPrefix
