package keychain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	store := value
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

// The deployment resource and the app-side Manager are two handles over one
// credential directory, and the index behind it is a read-modify-write. They
// must therefore share one lock: two backends would let concurrent Sets each
// rewrite accounts.json from their own snapshot and drop an entry.
func TestNewStoreSharesOneBackendPerDir(t *testing.T) {
	dir := t.TempDir()
	deployed, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	manager := NewManager(dir)
	if !manager.Available() {
		t.Fatal("manager over a usable directory is unavailable")
	}
	if deployed.backend != manager.store.backend {
		t.Fatal("manager opened a second backend over the same directory")
	}
	// A second spelling of one directory is the same store, not another one.
	alias, err := NewStore(dir + string(filepath.Separator) + ".")
	if err != nil {
		t.Fatalf("NewStore(alias): %v", err)
	}
	if alias.backend != deployed.backend {
		t.Fatal("a second spelling of the directory opened a second backend")
	}
	// A symlink is the same directory too, not another store with another
	// lock: the spelling a document uses is not what the store is keyed by.
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, link); err != nil {
		t.Logf("symlink unavailable, skipping that leg: %v", err)
	} else {
		linked, err := NewStore(link)
		if err != nil {
			t.Fatalf("NewStore(symlink): %v", err)
		}
		if linked.backend != deployed.backend {
			t.Fatal("a symlinked spelling of the directory opened a second backend")
		}
	}

	const accounts = 16
	var wg sync.WaitGroup
	for i := range accounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handle := deployed
			if i%2 == 0 {
				handle = manager.store
			}
			if setErr := handle.Set(context.Background(), fmt.Sprintf("acct-%d", i), "v"); setErr != nil {
				t.Errorf("Set(acct-%d): %v", i, setErr)
			}
		}()
	}
	wg.Wait()

	indexed, err := deployed.backend.(*fileBackend).readAccounts()
	if err != nil {
		t.Fatalf("readAccounts: %v", err)
	}
	if len(indexed) != accounts {
		t.Fatalf("index holds %d accounts, want %d: %v", len(indexed), accounts, indexed)
	}
}

// The key file on disk is the source of truth. When a store is reset under a
// running process — directory and key removed, opened again — the process
// keeps the same backend, and that backend must seal with the key the
// directory now holds, or the next process finds files it cannot open.
func TestNewStoreAdoptsTheKeyOnDisk(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewStore(dir); err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, encKeyFile)); err != nil {
		t.Fatalf("remove key file: %v", err)
	}
	reopened, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore after the reset: %v", err)
	}
	if err := reopened.Set(context.Background(), "acct", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// What the next process builds: a backend over the key on disk.
	restarted, err := loadOrCreateKey(dir)
	if err != nil {
		t.Fatalf("loadOrCreateKey: %v", err)
	}
	got, found, err := (&fileBackend{dir: dir, key: restarted}).Get(
		context.Background(), "acct")
	if err != nil || !found || got != "value" {
		t.Fatalf("Get from a fresh backend = (%q, %v, %v), "+
			"want the value sealed under the key the directory holds", got, found, err)
	}
}

// A key taken from disk waits for the operations still running under the old
// one: a Set that sealed under the previous key and published after the swap
// would leave a file this process can no longer open, and that the next
// process — which only knows the new key — cannot open either.
func TestKeyReplacementWaitsForInFlightOperation(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	backend := store.backend.(*fileBackend)
	backend.keyMu.RLock() // stand in for an operation that pinned the key
	reopened := make(chan struct{})
	go func() {
		defer close(reopened)
		if _, openErr := NewStore(dir); openErr != nil {
			t.Errorf("NewStore: %v", openErr)
		}
	}()
	// The reopen has to be blocked here; a scheduler slow enough to not run
	// the goroutine at all makes this assertion pass without proving
	// anything, but it can never fail the test on a correct fix.
	select {
	case <-reopened:
		t.Fatal("a reopen replaced the key while an operation held it")
	case <-time.After(100 * time.Millisecond):
	}
	backend.keyMu.RUnlock()
	select {
	case <-reopened:
	case <-time.After(10 * time.Second):
		t.Fatal("the reopen stayed blocked after the operation released the key")
	}
}

// One directory, two spellings: EvalSymlinks settles symlinks, but a
// case-insensitive volume still lets two strings name one directory, and two
// backends over one directory is the lost-index-entry bug the shared backend
// exists to prevent.
func TestNewStoreSharesOneBackendAcrossCaseSpellings(t *testing.T) {
	dir := t.TempDir()
	other := strings.ToUpper(dir)
	if _, err := os.Stat(other); err != nil || other == dir {
		t.Skip("file system is case-sensitive; two spellings cannot name one directory here")
	}
	deployed, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	same, err := NewStore(other)
	if err != nil {
		t.Fatalf("NewStore(other case): %v", err)
	}
	if same.backend != deployed.backend {
		t.Fatal("a case variant of the directory opened a second backend")
	}
}

// A removed directory gives up its store: a temporary filesystem that hands
// the freed inode number to the next directory must not hand the old store
// over with it, or the new store writes into a path that does not exist.
func TestNewStoreDoesNotInheritARemovedDirectory(t *testing.T) {
	gone := t.TempDir()
	stale, err := NewStore(gone)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	fresh := t.TempDir()
	store, err := NewStore(fresh)
	if err != nil {
		t.Fatalf("NewStore(fresh): %v", err)
	}
	if store.backend == stale.backend {
		t.Fatal("a store opened over a removed directory was handed to a new one")
	}
	want, err := filepath.EvalSymlinks(fresh)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got := store.backend.(*fileBackend).dir; got != want {
		t.Fatalf("new store is rooted at %q, want %q", got, want)
	}
	if err := store.Set(context.Background(), "acct", "value"); err != nil {
		t.Fatalf("Set: %v", err)
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
	value, err := f.Build(ctx, resource.NewInput(raw, nil, nil, nil))
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
