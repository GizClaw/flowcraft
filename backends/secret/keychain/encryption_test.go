package keychain

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestFileBackendEncryptsAtRest(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if !s.Available() {
		t.Fatal("store should be available")
	}
	if err := s.Set(context.Background(), "provider/inst-a", "sk-encrypted"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// The file on disk must be sealed, not plaintext.
	raw, err := os.ReadFile(s.backend.(*fileBackend).path("provider/inst-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, encMagic) {
		t.Fatalf("stored file does not start with magic %q: %q", encMagic, raw)
	}
	if bytes.Contains(raw, []byte("sk-encrypted")) {
		t.Fatal("plaintext leaked into the stored file")
	}

	got, found, err := s.Lookup(context.Background(), "provider/inst-a")
	if err != nil || !found || got != "sk-encrypted" {
		t.Fatalf("Lookup = (%q, %v, %v), want sk-encrypted", got, found, err)
	}
}

func TestFileBackendRejectsUnencryptedFile(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	b := s.backend.(*fileBackend)
	// Simulate an unencrypted file.
	if err := os.WriteFile(b.path("provider/plain"), []byte("sk-plain"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.Lookup(context.Background(), "provider/plain"); err == nil || found {
		t.Fatalf("Lookup(plain) = (found=%v, err=%v), want rejection", found, err)
	}
	// The plaintext file must remain untouched: no silent rewrite.
	raw, err := os.ReadFile(b.path("provider/plain"))
	if err != nil || string(raw) != "sk-plain" {
		t.Fatalf("unencrypted file was modified: %q, %v", raw, err)
	}
}

func TestFileBackendWithoutKeyIsUnavailable(t *testing.T) {
	b := &fileBackend{dir: t.TempDir()}
	if b.Available() {
		t.Fatal("backend without a key must be unavailable")
	}
	if err := b.Set(context.Background(), "x", "v"); err == nil {
		t.Fatal("Set without a key must fail: plaintext writes are forbidden")
	}
}

func TestFileBackendWrongKeyFails(t *testing.T) {
	dir := t.TempDir()
	k1 := make([]byte, encKeyLen)
	k2 := make([]byte, encKeyLen)
	if _, err := rand.Read(k1); err != nil {
		t.Fatal(err)
	}
	copy(k2, k1)
	k2[0] ^= 0x01
	b1 := &fileBackend{dir: dir, key: k1}
	b2 := &fileBackend{dir: dir, key: k2}
	if err := b1.Set(context.Background(), "a", "sk-x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b2.Get(context.Background(), "a"); err == nil {
		t.Fatal("decrypt with a wrong key should fail")
	}
}

func TestFileBackendTruncatedSealedRejected(t *testing.T) {
	b := newKeyedBackend(t, t.TempDir())
	raw := append([]byte{}, encMagic...)
	raw = append(raw, 0x01, 0x02) // shorter than a nonce
	if err := os.WriteFile(b.path("x"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Get(context.Background(), "x"); err == nil {
		t.Fatal("truncated sealed secret must be rejected")
	}
}

func TestKeyFileAndDirPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not carry POSIX permission bits")
	}
	dir := filepath.Join(t.TempDir(), "store")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, encKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Fatalf("store dir mode = %o, want 700", perm)
	}
	// Reopening the same dir must reuse the same key.
	again, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore again: %v", err)
	}
	if !bytes.Equal(s.backend.(*fileBackend).key, again.backend.(*fileBackend).key) {
		t.Fatal("second open used a different key")
	}
}

// Modes are re-asserted, not assumed: a store directory created by hand
// (or by an older tool) with looser permissions is tightened on open,
// and a sealed file carrying looser bits is republished 0600.
func TestStoreReassertsModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not carry POSIX permission bits")
	}
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("store dir mode = %o, want 700", perm)
	}
	ctx := context.Background()
	if err := s.Set(ctx, "account", "secret"); err != nil {
		t.Fatal(err)
	}
	sealed := s.backend.(*fileBackend).path("account")
	if err := os.Chmod(sealed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "account", "secret-2"); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("sealed file mode = %o, want 600 after a rewrite", perm)
	}
}

// A store whose key file was lost while sealed files remain must fail
// closed: minting a fresh key would make every stored secret unreadable
// and let Set overwrite them under the new key.
func TestStoreRefusesToMintKeyOverExistingSecrets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := s.Set(context.Background(), "account", "secret"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, encKeyFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir); !errors.Is(err, errKeyMissing) {
		t.Fatalf("NewStore with a lost key = %v, want errKeyMissing", err)
	}
	// The sealed secret and the index are untouched: nothing was
	// rewritten under a fresh key.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("store holds %d files, want the sealed secret and the index", len(entries))
	}
}

// Only store data blocks minting: a directory that holds unrelated
// files (a shared secrets folder) still gets its key.
func TestStoreMintsKeyBesideForeignFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("not ours"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir); err != nil {
		t.Fatalf("NewStore beside foreign files: %v", err)
	}
}

// Concurrent first runs must all end up on the same key: O_EXCL decides
// the winner and the losers wait out its write instead of reading the
// claimed-but-empty key file.
func TestStoreConcurrentFirstRunSharesOneKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	const runs = 8
	stores := make([]Store, runs)
	errs := make([]error, runs)
	var wg sync.WaitGroup
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stores[i], errs[i] = NewStore(dir)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("NewStore #%d: %v", i, err)
		}
	}
	ctx := context.Background()
	if err := stores[0].Set(ctx, "account", "secret"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < runs; i++ {
		got, found, err := stores[i].Lookup(ctx, "account")
		if err != nil || !found || got != "secret" {
			t.Fatalf("store #%d reads %q/%v/%v, want the winner's key", i, got, found, err)
		}
	}
}

// The loser of a concurrent first run can observe the claimed but empty
// key file; it waits for the winner's bytes instead of failing on a key
// that does not decode yet.
func TestReadKeyRetryingWaitsForTheWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, encKeyFile)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, encKeyLen)
	for i := range key {
		key[i] = byte(i)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600)
	}()
	got, err := readKeyRetrying(path)
	if err != nil {
		t.Fatalf("readKeyRetrying: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatal("readKeyRetrying returned the wrong key")
	}
}
