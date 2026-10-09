// Package keychain implements the secret.Store/keychain deployment
// backend: an encrypted, file-based credential store that keeps one
// 0600 file per secret under a 0700 directory on every platform.
//
// The format is deliberately the Linux approach — no native keychain
// ACLs, no authorization prompts, no cgo. Secrets are sealed with
// AES-256-GCM under a machine-local 32-byte key stored beside them as
// .key (0600). Key and ciphertext share one user-owned directory, so
// the store protects against accidental exposure — backups, file
// sharing, casual reads — rather than against another process running
// as the same user; a true OS credential store (Keychain, libsecret,
// DPAPI) would be a different backend.
//
// Sealed files carry an "ocenc1:" magic prefix; a file without it is
// rejected instead of being read as plaintext. The magic predates this
// package and is kept so stores written before the extraction keep
// working.
//
// [Register] adds the secret.Store/keychain factory to a resource
// registry, making the store configurable from a deployment document:
//
//	resources:
//	  keychain:
//	    kind: secret.Store
//	    impl: keychain
//	    settings:
//	      dir: /path/to/secrets
//
// [NewStore] opens the same store for callers holding the directory
// directly, and [Manager] is the never-failing app-side handle
// (settings pages, CLIs) built on top of it.
package keychain

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils/fsatomic"
)

// encMagic prefixes every sealed secret file. Files without it are
// rejected rather than treated as plaintext.
var encMagic = []byte("ocenc1:")

const (
	// encKeyLen is the AES-256 key size in bytes.
	encKeyLen = 32
	// encKeyFile is the machine-local key file inside the store dir.
	encKeyFile = ".key"
)

// Store is the built secret.Store value: a credential backend plus the
// deployment flags (id / default) carried from settings. It implements
// resource.SecretStore for lazy ${secret:...} resolution and exposes
// Set/Delete/DeletePrefix for app-side management.
type Store struct {
	backend backend
	id      string
	def     bool
}

// Settings is the settings subtree of the secret.Store/keychain
// resource.
type Settings struct {
	// ID is the short name used in ${secret:ID.NAME} references; empty
	// falls back to the deployment resource name.
	ID string `json:"id,omitempty"`
	// Default marks this store as the target of NAME-only
	// ${secret:NAME} references. At most one store in a deployment may
	// be default.
	Default bool `json:"default,omitempty"`
	// Dir is the credential store directory (0700). Required; it is
	// created on first use.
	Dir string `json:"dir"`
}

// backend abstracts the storage behind a Store. The file backend is the
// only implementation; a future OS credential store (Keychain,
// libsecret, DPAPI) would plug in here.
type backend interface {
	Get(ctx context.Context, name string) (value string, found bool, err error)
	Set(ctx context.Context, name, value string) error
	Delete(ctx context.Context, name string) error
	DeletePrefix(ctx context.Context, prefix string) error
	Available() bool
}

// NewStore opens the file backend rooted at dir, creating the store
// directory (0700) and the AES key file (.key, 0600) on first use. The
// store is sealed-only: with no usable key nothing is read or written.
func NewStore(dir string) (Store, error) {
	if strings.TrimSpace(dir) == "" {
		return Store{}, errdefs.Validationf(
			"secret keychain store: settings.dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Store{}, fmt.Errorf(
			"secret keychain store: create store dir: %w", err)
	}
	// MkdirAll is a no-op on an existing directory, so the mode is
	// re-asserted: the store's promise is 0700, however the directory
	// was created.
	if err := os.Chmod(dir, 0o700); err != nil {
		return Store{}, fmt.Errorf(
			"secret keychain store: secure store dir: %w", err)
	}
	key, err := loadOrCreateKey(dir)
	if err != nil {
		return Store{}, err
	}
	return Store{backend: &fileBackend{dir: dir, key: key}}, nil
}

// Lookup implements resource.SecretStore.
func (s Store) Lookup(ctx context.Context, name string) (string, bool, error) {
	if s.backend == nil {
		return "", false, errors.New("secret keychain store: store is unavailable")
	}
	return s.backend.Get(ctx, name)
}

// DefaultSecretStore implements resource.SecretStore.
func (s Store) DefaultSecretStore() bool { return s.def }

// SecretStoreID implements resource.SecretStoreID. Empty falls back to
// the deployment resource name.
func (s Store) SecretStoreID() string { return s.id }

// Available reports whether the backend can serve reads and writes.
func (s Store) Available() bool {
	return s.backend != nil && s.backend.Available()
}

// Set stores one secret.
func (s Store) Set(ctx context.Context, name, value string) error {
	if s.backend == nil {
		return errors.New("secret keychain store: store is unavailable")
	}
	return s.backend.Set(ctx, name, value)
}

// Delete removes one secret. A missing item is not an error.
func (s Store) Delete(ctx context.Context, name string) error {
	if s.backend == nil {
		return errors.New("secret keychain store: store is unavailable")
	}
	return s.backend.Delete(ctx, name)
}

// DeletePrefix removes every secret whose account starts with prefix.
// An empty prefix is rejected: it matches every account.
func (s Store) DeletePrefix(ctx context.Context, prefix string) error {
	if s.backend == nil {
		return errors.New("secret keychain store: store is unavailable")
	}
	return s.backend.DeletePrefix(ctx, prefix)
}

// Manager is the app-side credential handle sharing the same backend as
// the deploy resource. It never fails to construct: an unusable
// directory surfaces through Available and per-call errors.
type Manager struct {
	store Store
}

// NewManager returns a manager rooted at dir (the encrypted file
// backend). An unusable directory yields an unavailable manager rather
// than an error, so settings UIs can degrade gracefully.
func NewManager(dir string) *Manager {
	store, err := NewStore(dir)
	if err != nil {
		// Keep a zero store so callers degrade gracefully instead of
		// handling a construction error.
		return &Manager{}
	}
	return &Manager{store: store}
}

// Available reports whether the underlying backend is usable.
func (m *Manager) Available() bool {
	return m != nil && m.store.Available()
}

// Get returns one secret.
func (m *Manager) Get(ctx context.Context, account string) (string, bool, error) {
	if m == nil {
		return "", false, errors.New("secret keychain store: manager is unavailable")
	}
	return m.store.Lookup(ctx, account)
}

// Set stores one secret.
func (m *Manager) Set(ctx context.Context, account, value string) error {
	if m == nil {
		return errors.New("secret keychain store: manager is unavailable")
	}
	return m.store.Set(ctx, account, value)
}

// Delete removes one secret.
func (m *Manager) Delete(ctx context.Context, account string) error {
	if m == nil {
		return errors.New("secret keychain store: manager is unavailable")
	}
	return m.store.Delete(ctx, account)
}

// DeletePrefix removes every secret whose account starts with prefix
// (for example when a plugin or a deployment is removed). An empty
// prefix is rejected.
func (m *Manager) DeletePrefix(ctx context.Context, prefix string) error {
	if m == nil {
		return errors.New("secret keychain store: manager is unavailable")
	}
	return m.store.DeletePrefix(ctx, prefix)
}

// fileBackend stores one 0600 file per secret under a 0700 directory.
// File names are sha256 hashes of the account so arbitrary names can
// never escape the directory. Every file is sealed with AES-256-GCM
// (magic + random nonce + ciphertext+tag) under the store's 32-byte
// key; a backend without a key is unusable.
//
// The account index (accounts.json) is the only way to enumerate
// accounts, so it is guarded twice: modifications hold mu (two
// concurrent Sets must not drop an entry), and an index that exists but
// cannot be decoded is an error everywhere instead of an empty list
// (rewriting from empty would orphan every secret it still names).
// Writers in other processes are not serialized; management is an
// app-side, single-writer surface.
type fileBackend struct {
	dir string
	key []byte

	mu sync.Mutex // guards the accounts.json read-modify-write
}

// accountsFile tracks every account name so prefix deletion can find
// files (filenames are sha256 hashes and cannot be enumerated).
const accountsFile = "accounts.json"

func (f *fileBackend) Available() bool {
	return f != nil && f.dir != "" && len(f.key) == encKeyLen
}

func (f *fileBackend) path(name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(f.dir, hex.EncodeToString(sum[:]))
}

// readAccounts returns the account index. A missing index is an empty
// one (a fresh store); an index that exists but cannot be read or
// decoded is an error, never an empty list.
func (f *fileBackend) readAccounts() ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(f.dir, accountsFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"secret keychain store: read account list: %w", err)
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf(
			"secret keychain store: account list is undecodable, refusing to rewrite it: %w",
			err)
	}
	return list, nil
}

func (f *fileBackend) writeAccounts(list []string) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	// Atomic publish: the index is the only enumeration source, so a
	// crash mid-write must never truncate it.
	if err := fsatomic.Write(filepath.Join(f.dir, accountsFile), raw, fsatomic.Options{
		Perm:       0o600,
		TempPrefix: ".accounts-*.tmp",
	}); err != nil {
		return fmt.Errorf("secret keychain store: write account list: %w", err)
	}
	return nil
}

func (f *fileBackend) addAccount(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	list, err := f.readAccounts()
	if err != nil {
		return err
	}
	for _, n := range list {
		if n == name {
			return nil
		}
	}
	return f.writeAccounts(append(list, name))
}

func (f *fileBackend) removeAccount(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	list, err := f.readAccounts()
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(list))
	for _, n := range list {
		if n != name {
			kept = append(kept, n)
		}
	}
	return f.writeAccounts(kept)
}

// Get returns the stored value exactly as Set received it; this store
// only ever reads files it sealed itself, so there is nothing to
// normalize away (the plaintext file store trims because its files are
// hand-written).
func (f *fileBackend) Get(_ context.Context, name string) (string, bool, error) {
	raw, err := os.ReadFile(f.path(name))
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf(
			"secret keychain store: read %q: %w", name, err)
	}
	if bytes.HasPrefix(raw, encMagic) {
		plain, err := f.decrypt(raw)
		if err != nil {
			return "", false, fmt.Errorf(
				"secret keychain store: read %q: %w", name, err)
		}
		return string(plain), true, nil
	}
	// Unencrypted files are rejected, not silently read: the secret
	// must be re-entered so it is never left (or treated) as plaintext.
	return "", false, fmt.Errorf(
		"secret keychain store: %q is not encrypted; re-enter the secret", name)
}

func (f *fileBackend) Set(_ context.Context, name, value string) error {
	data, err := f.seal([]byte(value))
	if err != nil {
		return fmt.Errorf("secret keychain store: encrypt %q: %w", name, err)
	}
	// Atomic publish: a crash mid-write must not leave a truncated
	// sealed file (GCM would fail every later read), and the
	// temp-plus-rename path also settles the mode of a file written by
	// earlier tooling with looser permissions.
	if err := fsatomic.Write(f.path(name), data, fsatomic.Options{
		Perm:       0o600,
		TempPrefix: ".sealed-*.tmp",
	}); err != nil {
		return fmt.Errorf("secret keychain store: write %q: %w", name, err)
	}
	if err := f.addAccount(name); err != nil {
		return fmt.Errorf("secret keychain store: record account %q: %w", name, err)
	}
	return nil
}

func (f *fileBackend) Delete(_ context.Context, name string) error {
	if err := os.Remove(f.path(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("secret keychain store: delete %q: %w", name, err)
	}
	if err := f.removeAccount(name); err != nil {
		return fmt.Errorf("secret keychain store: record account removal %q: %w", name, err)
	}
	return nil
}

// DeletePrefix removes every secret whose account starts with prefix.
// An empty prefix is rejected: every account matches it, and this is
// the cleanup hook a "the plugin or deployment is gone" path calls.
func (f *fileBackend) DeletePrefix(_ context.Context, prefix string) error {
	if strings.TrimSpace(prefix) == "" {
		return errdefs.Validationf(
			"secret keychain store: refusing to delete every secret with an empty prefix")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	list, err := f.readAccounts()
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(list))
	for _, name := range list {
		if strings.HasPrefix(name, prefix) {
			if err := os.Remove(f.path(name)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("secret keychain store: delete %q: %w", name, err)
			}
			continue
		}
		kept = append(kept, name)
	}
	return f.writeAccounts(kept)
}

// seal encrypts value with AES-256-GCM under f.key. A missing key is
// an error: plaintext writes are never allowed.
func (f *fileBackend) seal(value []byte) ([]byte, error) {
	if len(f.key) == 0 {
		return nil, errors.New("secret keychain store: no encryption key")
	}
	gcm, err := newGCM(f.key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, value, nil)
	out := make([]byte, 0, len(encMagic)+len(nonce)+len(sealed))
	out = append(out, encMagic...)
	out = append(out, nonce...)
	out = append(out, sealed...)
	return out, nil
}

// decrypt reverses seal. Files without the magic prefix are rejected
// by Get before this runs.
func (f *fileBackend) decrypt(raw []byte) ([]byte, error) {
	if len(f.key) == 0 {
		return nil, errors.New("secret keychain store: no encryption key")
	}
	body := raw[len(encMagic):]
	gcm, err := newGCM(f.key)
	if err != nil {
		return nil, err
	}
	if len(body) < gcm.NonceSize() {
		return nil, errors.New("secret keychain store: truncated sealed secret")
	}
	nonce, sealed := body[:gcm.NonceSize()], body[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("secret keychain store: decrypt: %w", err)
	}
	return plain, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != encKeyLen {
		return nil, fmt.Errorf(
			"secret keychain store: key must be %d bytes, got %d",
			encKeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// errKeyMissing reports a store whose key file is gone while sealed
// secrets remain: minting a fresh key would silently make every one of
// them unreadable, and Set would overwrite them under the new key.
var errKeyMissing = errors.New(
	"secret keychain store: the store holds sealed secrets but its key file is missing; restore it from backup instead of starting fresh")

// loadOrCreateKey reads the store's 32-byte AES key, creating it with
// 0600 permissions on first use. Creation is claimed with O_EXCL, so a
// concurrent first run has exactly one winner; the loser waits out the
// winner's write instead of reading a half-written key. A store that
// already holds sealed secrets never mints a key: it fails closed.
func loadOrCreateKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, encKeyFile)
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if key, derr := decodeKey(raw); derr == nil {
			return key, nil
		}
		// The file exists but does not decode yet: a concurrent first
		// run may still be writing it (O_EXCL claims the name before
		// the bytes land). Wait for that writer; a genuinely truncated
		// key file still fails closed.
		return readKeyRetrying(path)
	case !os.IsNotExist(err):
		return nil, err
	}
	if err := ensureKeyMintable(dir); err != nil {
		return nil, err
	}
	key := make([]byte, encKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return readKeyRetrying(path)
		}
		return nil, err
	}
	// A failure past this point removes the claimed file again, so a
	// later run can mint cleanly instead of tripping over a key file
	// that will never decode.
	write := func() error {
		if err := fd.Chmod(0o600); err != nil {
			return err
		}
		if _, err := fd.Write([]byte(hex.EncodeToString(key) + "\n")); err != nil {
			return err
		}
		return fd.Sync()
	}
	if err := write(); err != nil {
		_ = fd.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := fd.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return key, nil
}

// readKeyRetrying reads the key a concurrent first run may still be
// writing: O_EXCL claims the file before it holds bytes, so a loser can
// observe an empty or partial key for a moment.
func readKeyRetrying(path string) ([]byte, error) {
	const attempts = 100
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		raw, err := os.ReadFile(path)
		if err != nil {
			lastErr = err
		} else if key, derr := decodeKey(raw); derr == nil {
			return key, nil
		} else {
			lastErr = derr
		}
		time.Sleep(2 * time.Millisecond)
	}
	return nil, fmt.Errorf(
		"secret keychain store: key file did not become usable: %w", lastErr)
}

// ensureKeyMintable refuses a fresh key when the directory already
// holds store data. Anything else in the directory is not the store's
// to judge.
func ensureKeyMintable(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == encKeyFile {
			continue
		}
		if entry.Name() == accountsFile {
			return errKeyMissing
		}
		file, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		head := make([]byte, len(encMagic))
		n, _ := io.ReadFull(file, head)
		_ = file.Close()
		if n == len(encMagic) && bytes.Equal(head, encMagic) {
			return errKeyMissing
		}
	}
	return nil
}

func decodeKey(raw []byte) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf(
			"secret keychain store: decode key file: %w", err)
	}
	if len(key) != encKeyLen {
		return nil, fmt.Errorf(
			"secret keychain store: key file must hold %d bytes, got %d",
			encKeyLen, len(key))
	}
	return key, nil
}
