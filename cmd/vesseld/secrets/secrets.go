package secrets

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Provider resolves a URL-shaped reference to the plaintext bytes
// the secret backend hands back. Implementations MUST:
//
//   - Reject malformed references (wrong scheme, missing components)
//     with errdefs.Validation so operators see the typo at boot.
//   - Surface "the backend isn't reachable / isn't implemented yet"
//     as errdefs.NotAvailable so callers can decide whether to fail
//     fast or fall back.
//   - Surface "the credential exists but I am not allowed to read
//     it" (e.g. file permissions disallow access) as
//     errdefs.Forbidden.
//   - Surface "the credential doesn't exist" as errdefs.NotFound.
//
// The classification matters because the daemon's startup path maps
// these to distinct operator-visible messages and exit codes.
type Provider interface {
	Get(ctx context.Context, ref string) ([]byte, error)
}

// Multi routes Get to the registered backend selected by ref's URL
// scheme. The router itself owns no state beyond the scheme map;
// thread-safety for backends is each backend's concern (the stock
// EnvProvider / FileProvider / VaultProvider are stateless and
// therefore safe for concurrent use).
type Multi struct {
	backends map[string]Provider
}

// NewMulti returns an empty router. Register backends before calling
// Get, otherwise every reference resolves to errdefs.Validation
// ("unknown scheme"). Most callers want [Default] instead.
func NewMulti() *Multi {
	return &Multi{backends: make(map[string]Provider)}
}

// Register associates scheme with backend p. Re-registering the same
// scheme overwrites the previous backend; this is the seam through
// which tests inject fakes and operators wire alternate Vault clients.
//
// scheme is normalised to lower-case to match net/url behaviour.
// Empty schemes are rejected because the URL spec requires one to
// disambiguate routing.
func (m *Multi) Register(scheme string, p Provider) error {
	if scheme == "" {
		return errdefs.Validationf("secrets: cannot register an empty scheme")
	}
	if p == nil {
		return errdefs.Validationf("secrets: cannot register a nil Provider for scheme %q", scheme)
	}
	m.backends[strings.ToLower(scheme)] = p
	return nil
}

// Get parses ref to extract the scheme, then dispatches to the
// matching backend. Returns errdefs.Validation when ref is not a
// URL or the scheme has no registered backend.
func (m *Multi) Get(ctx context.Context, ref string) ([]byte, error) {
	scheme, err := schemeOf(ref)
	if err != nil {
		return nil, err
	}
	backend, ok := m.backends[scheme]
	if !ok {
		return nil, errdefs.Validationf(
			"secrets: no backend registered for scheme %q (ref %q)", scheme, ref)
	}
	return backend.Get(ctx, ref)
}

// Schemes returns the registered scheme list in deterministic order.
// Useful for diagnostic output ("vesseld: secrets backends ready: env,
// file, vault") and for tests that want to assert wiring.
func (m *Multi) Schemes() []string {
	out := make([]string, 0, len(m.backends))
	for s := range m.backends {
		out = append(out, s)
	}
	// Simple insertion sort keeps the dep surface zero (avoids
	// pulling in sort just for diagnostics).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Default returns a Multi pre-registered with the three stock
// backends: env://, file://, vault://. The FileProvider is built in
// permissive mode (EnforceMode = true), matching what the daemon
// itself wants. Callers needing alternate posture should build a
// Multi by hand.
func Default() *Multi {
	m := NewMulti()
	_ = m.Register("env", NewEnvProvider())
	_ = m.Register("file", &FileProvider{EnforceMode: true})
	_ = m.Register("vault", &VaultProvider{})
	return m
}

// schemeOf extracts the lower-cased scheme from ref. Returns
// Validation when ref is empty, scheme-less, or malformed enough
// that url.Parse rejects it.
func schemeOf(ref string) (string, error) {
	if ref == "" {
		return "", errdefs.Validationf("secrets: empty reference")
	}
	// Use url.Parse so we get a single set of edge-case behaviours
	// (percent-encoding, IPv6 hosts, …) rather than handrolled
	// string splitting that would diverge from the URL spec.
	u, err := url.Parse(ref)
	if err != nil {
		return "", errdefs.Validationf("secrets: parse %q: %v", ref, err)
	}
	if u.Scheme == "" {
		return "", errdefs.Validationf("secrets: reference %q has no scheme (expected env://, file://, or vault://)", ref)
	}
	return strings.ToLower(u.Scheme), nil
}

// ---------------------------------------------------------------------------
// EnvProvider — env://NAME
// ---------------------------------------------------------------------------

// EnvProvider resolves references of the form env://NAME by reading
// the daemon process environment. Unset variables surface as
// errdefs.NotFound (vs. empty values, which return an empty []byte
// successfully — an empty secret is a legitimate, if unusual, value).
//
// Stateless; safe for concurrent use.
type EnvProvider struct{}

// NewEnvProvider is the conventional constructor; callers may also
// use &EnvProvider{} directly. Provided so the API shape matches
// FileProvider's, which has fields.
func NewEnvProvider() *EnvProvider { return &EnvProvider{} }

// Get reads ref's variable from os.Getenv. The variable name is
// taken from u.Host (env://NAME); env://NAME/path is rejected so
// operators do not accidentally write env://API_KEY/oops and get
// silently truncated lookups.
func (EnvProvider) Get(_ context.Context, ref string) ([]byte, error) {
	u, err := parseAndCheckScheme(ref, "env")
	if err != nil {
		return nil, err
	}
	name := u.Host
	if name == "" {
		// Handle env:NAME (without "//") shape gracefully: url.Parse
		// puts the rest in Opaque.
		name = u.Opaque
	}
	if name == "" {
		return nil, errdefs.Validationf("secrets: env reference %q is missing the variable name", ref)
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errdefs.Validationf(
			"secrets: env reference %q must be env://NAME (no path component)", ref)
	}
	if !validEnvName(name) {
		return nil, errdefs.Validationf(
			"secrets: env reference %q contains an invalid POSIX variable name %q", ref, name)
	}
	v, ok := os.LookupEnv(name)
	if !ok {
		return nil, errdefs.NotFoundf("secrets: environment variable %q is not set", name)
	}
	return []byte(v), nil
}

// validEnvName implements the POSIX "name" grammar: [A-Za-z_][A-Za-z0-9_]*.
// Tighter than what os.Getenv would actually accept; the strictness
// catches typos like env://API-KEY before they masquerade as a
// missing variable.
func validEnvName(s string) bool {
	for i, r := range s {
		switch {
		case 'A' <= r && r <= 'Z', 'a' <= r && r <= 'z', r == '_':
		case '0' <= r && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return s != ""
}

// ---------------------------------------------------------------------------
// FileProvider — file:///abs/path
// ---------------------------------------------------------------------------

// FileProvider resolves file:// references by reading from the host
// filesystem. References MUST be absolute; relative paths are
// rejected because the daemon's cwd is not a stable contract under
// systemd / k8s deployments — operators who want "relative to /etc"
// should write file:///etc/... explicitly.
//
// File-permission enforcement (EnforceMode) is on by default to
// catch the "secret committed with mode 0644" mistake at boot
// instead of after the credential leaks. The check is POSIX-style
// (perm bits & 0o077 must be zero); on Windows the bits returned by
// os.Stat are synthesised and unreliable, so EnforceMode is silently
// ignored there — caller-visible failures on Windows would be
// false positives.
type FileProvider struct {
	// EnforceMode rejects files readable by group or others
	// (`perm & 0o077 != 0`). Defaults to true via [Default]; tests
	// and shared-secret-volume scenarios may disable it explicitly.
	EnforceMode bool
}

// Get reads ref's file contents. Trailing CR / LF bytes are trimmed
// so a file written with a final newline returns the same plaintext
// as one written without — operators routinely copy-paste credentials
// and the inconsistency is the most common "my key doesn't work"
// failure mode in field deployments.
func (p *FileProvider) Get(_ context.Context, ref string) ([]byte, error) {
	u, err := parseAndCheckScheme(ref, "file")
	if err != nil {
		return nil, err
	}
	if u.Host != "" && u.Host != "localhost" {
		// file://host/path is valid RFC syntax for a remote host —
		// but the only host we support is the local one. Reject
		// anything else loudly.
		return nil, errdefs.Validationf(
			"secrets: file reference %q targets host %q; only file:///abs/path is supported",
			ref, u.Host)
	}
	path := u.Path
	if path == "" {
		return nil, errdefs.Validationf("secrets: file reference %q is missing the path component", ref)
	}
	if !filepath.IsAbs(path) {
		return nil, errdefs.Validationf(
			"secrets: file reference %q must be absolute (e.g. file:///etc/secret)", ref)
	}

	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errdefs.NotFoundf("secrets: file %q does not exist", path)
		}
		if os.IsPermission(err) {
			return nil, errdefs.Forbiddenf("secrets: stat %q: %v", path, err)
		}
		return nil, fmt.Errorf("secrets: stat %q: %w", path, err)
	}
	if fi.IsDir() {
		return nil, errdefs.Validationf("secrets: file reference %q points at a directory, not a file", path)
	}

	if p.EnforceMode && runtime.GOOS != "windows" {
		if leaked := fi.Mode().Perm() & 0o077; leaked != 0 {
			return nil, errdefs.Forbiddenf(
				"secrets: file %q has group/other-readable mode %v; chmod 600 (or pass EnforceMode=false) to use it",
				path, fi.Mode().Perm())
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsPermission(err) {
			return nil, errdefs.Forbiddenf("secrets: read %q: %v", path, err)
		}
		return nil, fmt.Errorf("secrets: read %q: %w", path, err)
	}
	// Trim a single trailing newline (LF or CRLF) so editors that
	// silently append one do not change the secret. Multi-line
	// secrets (PEM blocks, JWT tokens with embedded newlines) keep
	// their internal newlines intact.
	switch {
	case len(raw) >= 2 && raw[len(raw)-2] == '\r' && raw[len(raw)-1] == '\n':
		raw = raw[:len(raw)-2]
	case len(raw) >= 1 && raw[len(raw)-1] == '\n':
		raw = raw[:len(raw)-1]
	}
	return raw, nil
}

// ---------------------------------------------------------------------------
// VaultProvider — vault://server/path?key=... (stub)
// ---------------------------------------------------------------------------

// VaultProvider is the placeholder backend for HashiCorp Vault. It
// pins the reference shape so callers can write vault:// refs today
// (mTLS material, k8s secrets, sandbox env injection) and pick up
// real reads when the implementation lands.
//
// The shape we commit to is:
//
//	vault://<server>/<path>?key=<field>[&version=<n>]
//
// Resolution maps to a KV-v2 read of <path>, returning the <field>'s
// value; <version> defaults to the latest. The interface is fixed;
// the body just hasn't been wired to a Vault client yet.
//
// All Get calls return errdefs.NotAvailable so callers fail loudly
// when they try to use a vault:// ref against this stub instead of
// hitting a half-implemented codepath.
type VaultProvider struct{}

// Get always returns errdefs.NotAvailable. The reference is still
// parsed and validated so a malformed vault:// ref surfaces as
// errdefs.Validation at boot rather than as NotAvailable, letting
// operators distinguish "vault backend isn't ready yet" from
// "your YAML is wrong".
func (*VaultProvider) Get(_ context.Context, ref string) ([]byte, error) {
	u, err := parseAndCheckScheme(ref, "vault")
	if err != nil {
		return nil, err
	}
	if u.Host == "" {
		return nil, errdefs.Validationf("secrets: vault reference %q is missing the server host", ref)
	}
	if u.Path == "" || u.Path == "/" {
		return nil, errdefs.Validationf("secrets: vault reference %q is missing the secret path", ref)
	}
	if u.Query().Get("key") == "" {
		return nil, errdefs.Validationf("secrets: vault reference %q is missing required ?key=<field>", ref)
	}
	return nil, errdefs.NotAvailablef(
		"secrets: vault backend not yet implemented (ref %q is well-formed but cannot be read)", ref)
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// parseAndCheckScheme is the shared front-door for every backend's
// Get: parse the ref as a URL, confirm the scheme matches what the
// backend claims to own (defence against operators wiring the wrong
// backend under the wrong scheme), and return the parsed *url.URL.
//
// Backend-specific component checks (host vs. opaque, path layout,
// query-string fields) are the backend's own job.
func parseAndCheckScheme(ref, want string) (*url.URL, error) {
	if ref == "" {
		return nil, errdefs.Validationf("secrets: empty reference")
	}
	u, err := url.Parse(ref)
	if err != nil {
		return nil, errdefs.Validationf("secrets: parse %q: %v", ref, err)
	}
	if !strings.EqualFold(u.Scheme, want) {
		return nil, errdefs.Validationf(
			"secrets: reference %q has scheme %q; this backend handles %q://", ref, u.Scheme, want)
	}
	return u, nil
}
