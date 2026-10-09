package keychain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/secret"
)

// The store is consumed through the same contracts as the built-in
// env/file stores.
var (
	_ resource.SecretStore   = Store{}
	_ resource.SecretStoreID = Store{}
)

// newKeyedBackend builds a file backend with a real encryption key, so
// tests exercise the sealed format rather than the unusable nil-key
// state.
func newKeyedBackend(t *testing.T, dir string) *fileBackend {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key, err := loadOrCreateKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &fileBackend{dir: dir, key: key}
}

func TestFileBackendRoundTrip(t *testing.T) {
	b := newKeyedBackend(t, t.TempDir())
	if err := b.Set(context.Background(), "account-a", "value-a"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, found, err := b.Get(context.Background(), "account-a")
	if err != nil || !found || got != "value-a" {
		t.Fatalf("Get = (%q, %v, %v), want value-a", got, found, err)
	}
	if _, found, err := b.Get(context.Background(), "missing"); err != nil || found {
		t.Fatalf("Get(missing) = (%v, %v), want not found", found, err)
	}
	if err := b.Delete(context.Background(), "account-a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, err := b.Get(context.Background(), "account-a"); err != nil || found {
		t.Fatalf("Get after Delete = (%v, %v), want not found", found, err)
	}
}

func TestFileBackendNameCannotEscape(t *testing.T) {
	dir := t.TempDir()
	b := newKeyedBackend(t, dir)
	// Names with separators and traversal attempts must stay inside the
	// store directory (file names are sha256 hashes).
	for _, name := range []string{"../outside", "a/b", `a\b`, ".."} {
		if err := b.Set(context.Background(), name, "v"); err != nil {
			t.Fatalf("Set(%q): %v", name, err)
		}
		got, found, err := b.Get(context.Background(), name)
		if err != nil || !found || got != "v" {
			t.Fatalf("Get(%q) = (%q, %v, %v)", name, got, found, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 6 {
		// 4 secret files + accounts.json index + the encryption key.
		t.Fatalf("store dir has %d entries, want 6", len(entries))
	}
}

func TestStoreLookupAndFlags(t *testing.T) {
	b := newKeyedBackend(t, t.TempDir())
	if err := b.Set(context.Background(), "x", "secret-x"); err != nil {
		t.Fatal(err)
	}
	s := Store{backend: b, id: "keychain", def: true}
	if !s.DefaultSecretStore() || s.SecretStoreID() != "keychain" {
		t.Fatalf("flags = (%v, %q)", s.DefaultSecretStore(), s.SecretStoreID())
	}
	if !s.Available() {
		t.Fatal("file backend should be available")
	}
	got, found, err := s.Lookup(context.Background(), "x")
	if err != nil || !found || got != "secret-x" {
		t.Fatalf("Lookup = (%q, %v, %v)", got, found, err)
	}
}

func TestStoreUnavailable(t *testing.T) {
	var s Store
	if s.Available() {
		t.Fatal("zero store should be unavailable")
	}
	if s.DefaultSecretStore() {
		t.Fatal("zero store should not be default")
	}
	if _, _, err := s.Lookup(context.Background(), "x"); err == nil {
		t.Fatal("Lookup on zero store should error")
	}
	if err := s.Set(context.Background(), "x", "v"); err == nil {
		t.Fatal("Set on zero store should error")
	}
	if err := s.Delete(context.Background(), "x"); err == nil {
		t.Fatal("Delete on zero store should error")
	}
	if err := s.DeletePrefix(context.Background(), "x"); err == nil {
		t.Fatal("DeletePrefix on zero store should error")
	}
}

func TestFactoryRegistersKeychainImpl(t *testing.T) {
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatal(err)
	}
	f, ok := reg.Lookup(secret.ResourceKind, ResourceImpl)
	if !ok {
		t.Fatalf("registry missing %s/%s", secret.ResourceKind, ResourceImpl)
	}
	if f.Spec().Kind != secret.ResourceKind || f.Spec().Impl != ResourceImpl {
		t.Fatalf("spec = %+v", f.Spec())
	}
}

func TestFactoryNewDecodesSettings(t *testing.T) {
	dir := t.TempDir()
	raw, err := json.Marshal(map[string]any{
		"id": "keychain", "default": true, "dir": dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := factory{}.New(context.Background(), resource.Input{
		Settings: raw,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store, ok := value.(Store)
	if !ok {
		t.Fatalf("New returned %T, want keychain.Store", value)
	}
	if !store.DefaultSecretStore() || store.SecretStoreID() != "keychain" {
		t.Fatalf("decoded flags = (%v, %q)", store.DefaultSecretStore(), store.SecretStoreID())
	}
	if !store.Available() {
		t.Fatal("store should be available")
	}
}

func TestFactorySettingsValidation(t *testing.T) {
	_, err := factory{}.New(context.Background(), resource.Input{
		Settings: []byte(`{"default": true}`),
	})
	if !errdefs.IsValidation(err) {
		t.Fatalf("missing dir error = %v, want validation", err)
	}
	dir, err := json.Marshal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = factory{}.New(context.Background(), resource.Input{
		Settings: []byte(`{"dir": ` + string(dir) + `, "bogus": 1}`),
	})
	if err == nil {
		t.Fatal("New accepted unknown settings field")
	}
}

func TestManagerSetGetDelete(t *testing.T) {
	m := NewManager(t.TempDir())
	if !m.Available() {
		t.Fatal("manager should be available")
	}
	if err := m.Set(context.Background(), "provider/inst-a", "sk-x"); err != nil {
		t.Fatal(err)
	}
	got, found, err := m.Get(context.Background(), "provider/inst-a")
	if err != nil || !found || got != "sk-x" {
		t.Fatalf("Get = (%q, %v, %v)", got, found, err)
	}
	if err := m.Delete(context.Background(), "provider/inst-a"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := m.Get(context.Background(), "provider/inst-a"); found {
		t.Fatal("secret still present after Delete")
	}
}

func TestManagerUnavailable(t *testing.T) {
	if m := NewManager(""); m.Available() {
		t.Fatal("manager with no dir should be unavailable")
	}
	var nilManager *Manager
	if nilManager.Available() {
		t.Fatal("nil manager should be unavailable")
	}
	if _, _, err := nilManager.Get(context.Background(), "a"); err == nil {
		t.Fatal("Get on nil manager should error")
	}
	if err := nilManager.Set(context.Background(), "a", "b"); err == nil {
		t.Fatal("Set on nil manager should error")
	}
	if err := nilManager.Delete(context.Background(), "a"); err == nil {
		t.Fatal("Delete on nil manager should error")
	}
	if err := nilManager.DeletePrefix(context.Background(), "a"); err == nil {
		t.Fatal("DeletePrefix on nil manager should error")
	}
}

func TestDeletePrefix(t *testing.T) {
	b := newKeyedBackend(t, t.TempDir())
	ctx := context.Background()
	for _, acc := range []string{
		"plugin-a/token",
		"plugin-a/meta",
		"plugin-b/token",
		"provider/x",
	} {
		if err := b.Set(ctx, acc, "v"); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.DeletePrefix(ctx, "plugin-a/"); err != nil {
		t.Fatal(err)
	}
	for _, acc := range []string{"plugin-a/token", "plugin-a/meta"} {
		if _, found, err := b.Get(ctx, acc); err != nil || found {
			t.Fatalf("%q still present after prefix delete", acc)
		}
	}
	for _, acc := range []string{"plugin-b/token", "provider/x"} {
		if _, found, err := b.Get(ctx, acc); err != nil || !found {
			t.Fatalf("%q unexpectedly removed", acc)
		}
	}
	// Deleted accounts are gone from the index too.
	accounts, err := b.readAccounts()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range accounts {
		if n == "plugin-a/token" {
			t.Fatal("deleted account still indexed")
		}
	}
	first := false
	for _, n := range accounts {
		if n == "plugin-b/token" {
			first = true
		}
	}
	if !first {
		t.Fatal("kept account dropped from the index")
	}
}

// The file store returns exactly what Set received: unlike the
// plaintext file store there is nothing hand-written to normalize, and
// trimming would silently corrupt a value that ends in a newline (a PEM
// block, a password that ends in "\r\n").
func TestFileBackendGetReturnsStoredValueVerbatim(t *testing.T) {
	b := newKeyedBackend(t, t.TempDir())
	ctx := context.Background()
	for _, value := range []string{
		"v",
		"v\n",
		"v\r\n",
		"line1\nline2\n",
		"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n",
	} {
		if err := b.Set(ctx, "account", value); err != nil {
			t.Fatal(err)
		}
		got, found, err := b.Get(ctx, "account")
		if err != nil || !found {
			t.Fatalf("Get(%q): found=%v err=%v", value, found, err)
		}
		if got != value {
			t.Fatalf("Get = %q, want %q", got, value)
		}
	}
}

// DeletePrefix("") matches every account; it is rejected so the
// "the plugin or deployment is gone" path cannot wipe the store by
// accident.
func TestDeletePrefixRefusesEmptyPrefix(t *testing.T) {
	b := newKeyedBackend(t, t.TempDir())
	ctx := context.Background()
	for _, acc := range []string{"plugin-a/token", "provider/x"} {
		if err := b.Set(ctx, acc, "v"); err != nil {
			t.Fatal(err)
		}
	}
	err := b.DeletePrefix(ctx, "")
	if err == nil {
		t.Fatal("empty prefix must be rejected")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("error = %v, want a validation error", err)
	}
	for _, acc := range []string{"plugin-a/token", "provider/x"} {
		if _, found, err := b.Get(ctx, acc); err != nil || !found {
			t.Fatalf("%q removed by an empty prefix", acc)
		}
	}
}

// An index that cannot be decoded must not be treated as an empty one:
// every later rewrite would drop the accounts it still names, and the
// hashed file names cannot be enumerated again.
func TestUndecodableAccountIndexFailsClosed(t *testing.T) {
	b := newKeyedBackend(t, t.TempDir())
	ctx := context.Background()
	for _, acc := range []string{"plugin-a/token", "provider/x"} {
		if err := b.Set(ctx, acc, "v"); err != nil {
			t.Fatal(err)
		}
	}
	index := filepath.Join(b.dir, accountsFile)
	if err := os.WriteFile(index, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.Set(ctx, "new", "v"); err == nil {
		t.Fatal("Set must fail while the index is undecodable")
	}
	if err := b.DeletePrefix(ctx, "plugin-a/"); err == nil {
		t.Fatal("DeletePrefix must fail while the index is undecodable")
	}
	raw, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{not json" {
		t.Fatalf("undecodable index was rewritten: %q", raw)
	}
	got, found, err := b.Get(ctx, "plugin-a/token")
	if err != nil || !found || got != "v" {
		t.Fatalf("Get = %q/%v/%v, want the stored secret", got, found, err)
	}
}

func TestSecretResolverIntegration(t *testing.T) {
	ctx := context.Background()
	reg := resource.NewRegistry()
	if err := Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	f, ok := reg.Lookup(secret.ResourceKind, ResourceImpl)
	if !ok {
		t.Fatalf("factory %s/%s missing", secret.ResourceKind, ResourceImpl)
	}
	raw, err := json.Marshal(map[string]any{
		"id": "keychain", "default": true, "dir": t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	value, err := f.New(ctx, resource.Input{Settings: raw})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store := value.(Store)
	if err := store.Set(ctx, "api-key", "sk-live"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// The factory-built value plugs into the same ${secret:...}
	// resolution chain the built-in stores use.
	resolver := resource.NewSecretResolver(
		map[string]resource.SecretStore{"keychain": store}, "keychain")
	got, err := resolver.Resolve(ctx, "keychain", "api-key")
	if err != nil || got != "sk-live" {
		t.Fatalf("Resolve = (%q, %v), want sk-live", got, err)
	}
	if _, err := resolver.Resolve(ctx, "keychain", "missing"); err == nil {
		t.Fatal("missing secret should fail resolution")
	}
}
