package keychain

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
