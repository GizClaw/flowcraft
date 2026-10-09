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
	"os"
	"path/filepath"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
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
// (for example when a plugin or a deployment is removed).
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
type fileBackend struct {
	dir string
	key []byte
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

func (f *fileBackend) readAccounts() []string {
	raw, err := os.ReadFile(filepath.Join(f.dir, accountsFile))
	if err != nil {
		if !os.IsNotExist(err) {
			telemetry.WarnErr(context.Background(),
				"secret keychain store: read account list failed", err)
		}
		return nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err != nil {
		telemetry.WarnErr(context.Background(),
			"secret keychain store: decode account list failed", err)
		return nil
	}
	return list
}

func (f *fileBackend) writeAccounts(list []string) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(f.dir, accountsFile), raw, 0o600)
}

func (f *fileBackend) addAccount(name string) error {
	list := f.readAccounts()
	for _, n := range list {
		if n == name {
			return nil
		}
	}
	return f.writeAccounts(append(list, name))
}

func (f *fileBackend) removeAccount(name string) error {
	list := f.readAccounts()
	kept := list[:0]
	for _, n := range list {
		if n != name {
			kept = append(kept, n)
		}
	}
	return f.writeAccounts(kept)
}

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
		return strings.TrimRight(string(plain), "\r\n"), true, nil
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
	if err := os.WriteFile(f.path(name), data, 0o600); err != nil {
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

func (f *fileBackend) DeletePrefix(_ context.Context, prefix string) error {
	list := f.readAccounts()
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

// loadOrCreateKey reads the store's 32-byte AES key, creating it with
// 0600 permissions on first use. Concurrent first runs race on O_EXCL;
// the loser reads the winner's key.
func loadOrCreateKey(dir string) ([]byte, error) {
	path := filepath.Join(dir, encKeyFile)
	raw, err := os.ReadFile(path)
	if err == nil {
		return decodeKey(raw)
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	key := make([]byte, encKeyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	fd, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil, rerr
			}
			return decodeKey(raw)
		}
		return nil, err
	}
	defer func() {
		telemetry.WarnErr(context.Background(),
			"secret keychain store: close key file failed", fd.Close())
	}()
	if err := fd.Chmod(0o600); err != nil {
		return nil, err
	}
	if _, err := fd.Write([]byte(hex.EncodeToString(key) + "\n")); err != nil {
		return nil, err
	}
	if err := fd.Sync(); err != nil {
		return nil, err
	}
	return key, nil
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
