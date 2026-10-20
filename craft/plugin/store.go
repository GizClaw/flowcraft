package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/craft/internal/version"
)

// Root is one directory scanned for plugin subdirectories. Builtin
// roots are read-only: they can be disabled or shadowed by a user root
// with the same plugin id, never uninstalled.
type Root struct {
	Path    string
	Builtin bool
}

// Options configures a Store.
type Options struct {
	// Roots are scanned in order; a later root shadows an earlier one
	// with the same plugin id.
	Roots []Root
	// StateDir holds the enable state and plugin KV files.
	StateDir string
	// DataDirRoot holds one data directory per plugin.
	DataDirRoot string
	// HostVersion is reported to plugins and checked against
	// minHostVersion when non-empty.
	HostVersion string
}

// Entry is one scanned plugin.
type Entry struct {
	ID       string
	Dir      string
	Manifest Manifest
	Enabled  bool
	Builtin  bool
	// ShadowsBuiltin reports a user plugin overriding a builtin id.
	ShadowsBuiltin bool
	BuiltinVersion string
	// Error holds the scan/validation error for an invalid plugin.
	Error string
	// Conflict names why a valid plugin cannot be published: another
	// plugin claims one of the tool namespaces it would publish under.
	// It is separate from Error because the plugin is not invalid — its
	// manifest parses, its files are there — and the two states need
	// different recoveries: a plugin with a conflict can be disabled
	// (which is how a user resolves the collision), while an invalid one
	// can be neither enabled nor disabled through the store's valid
	// view.
	Conflict string
}

// Summary is the UI-facing view of one plugin.
type Summary struct {
	ID             string   `json:"id"`
	Name           string   `json:"name,omitempty"`
	Version        string   `json:"version,omitempty"`
	Entry          string   `json:"entry,omitempty"`
	Permissions    []string `json:"permissions,omitempty"`
	Enabled        bool     `json:"enabled"`
	Builtin        bool     `json:"builtin,omitempty"`
	ShadowsBuiltin bool     `json:"shadows_builtin,omitempty"`
	BuiltinVersion string   `json:"builtin_version,omitempty"`
	Error          string   `json:"error,omitempty"`
	// Conflict names the tool namespace another plugin already
	// publishes; the entry is listed, and cannot be enabled until one of
	// the two is disabled or renamed.
	Conflict string `json:"conflict,omitempty"`
}

// Store scans plugin roots, keeps the enable state and plugin KV data,
// and publishes a revision on every mutation.
type Store struct {
	opts Options

	mu       sync.Mutex
	revision uint64
	subs     map[uint64]func()
	nextSub  uint64
	kv       map[string]*KV
}

// NewStore opens a store over the given roots.
func NewStore(opts Options) (*Store, error) {
	if strings.TrimSpace(opts.StateDir) == "" {
		return nil, errdefs.Validationf("plugin store: StateDir is required")
	}
	if strings.TrimSpace(opts.DataDirRoot) == "" {
		return nil, errdefs.Validationf("plugin store: DataDirRoot is required")
	}
	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return nil, errdefs.Validationf(
			"plugin store: create state dir: %w", err)
	}
	if err := os.MkdirAll(opts.DataDirRoot, 0o700); err != nil {
		return nil, errdefs.Validationf(
			"plugin store: create data dir: %w", err)
	}
	return &Store{
		opts: opts,
		subs: make(map[uint64]func()),
		kv:   make(map[string]*KV),
	}, nil
}

// Options returns the store's configuration.
func (s *Store) Options() Options { return s.opts }

// Revision returns the current registry revision.
func (s *Store) Revision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

// Subscribe registers fn for revision changes and returns a cancel
// function.
func (s *Store) Subscribe(fn func()) func() {
	if fn == nil {
		return func() {}
	}
	s.mu.Lock()
	s.nextSub++
	id := s.nextSub
	s.subs[id] = fn
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		delete(s.subs, id)
		s.mu.Unlock()
	}
}

func (s *Store) bumpLocked() []func() {
	s.revision++
	out := make([]func(), 0, len(s.subs))
	for _, fn := range s.subs {
		out = append(out, fn)
	}
	return out
}

// List returns every scanned plugin, including invalid ones. A state
// file that could not be read is returned alongside the list: the
// plugins are still listed (so a UI can show them and the error), but
// their enabled flags are the fail-closed ones.
func (s *Store) List() ([]Summary, error) {
	entries, stateErr := s.scan()
	out := make([]Summary, 0, len(entries))
	for _, entry := range entries {
		summary := Summary{
			ID:             entry.ID,
			Enabled:        entry.Enabled,
			Builtin:        entry.Builtin,
			ShadowsBuiltin: entry.ShadowsBuiltin,
			BuiltinVersion: entry.BuiltinVersion,
			Error:          entry.Error,
			Conflict:       entry.Conflict,
		}
		if entry.Error == "" {
			summary.Name = entry.Manifest.Name
			summary.Version = entry.Manifest.Version
			summary.Entry = entry.Manifest.Entry()
			summary.Permissions = append(
				[]string(nil), entry.Manifest.Permissions...)
		}
		out = append(out, summary)
	}
	return out, stateErr
}

// Entries returns every valid scanned plugin. An unreadable state file
// is an error: nothing may act on the fail-closed view as if it were
// the recorded one.
func (s *Store) Entries() ([]Entry, error) {
	entries, stateErr := s.scan()
	if stateErr != nil {
		return nil, stateErr
	}
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Error == "" {
			out = append(out, entry)
		}
	}
	return out, nil
}

// Enabled returns every valid, enabled plugin that can be published: a
// plugin whose tool namespace another plugin already claims is not in
// the list, because starting it would publish tools under a name that
// does not resolve. The entry stays readable (and disable-able) through
// [Store.List] and [Store.Entry].
func (s *Store) Enabled() ([]Entry, error) {
	entries, err := s.Entries()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if entry.Enabled && entry.Conflict == "" {
			out = append(out, entry)
		}
	}
	return out, nil
}

// Entry returns one valid plugin by id. A plugin whose tool namespace
// collides with another's is returned too: it is valid, it is readable,
// and the conflict travels with it (see [Entry.Conflict]) — a caller
// that would start it has to refuse it, and a caller that would disable
// it must be able to find it.
func (s *Store) Entry(id string) (Entry, bool) {
	entries, _ := s.scan()
	for _, entry := range entries {
		if entry.ID == id && entry.Error == "" {
			return entry, true
		}
	}
	return Entry{}, false
}

// SetEnabled toggles one plugin and bumps the revision.
func (s *Store) SetEnabled(id string, enabled bool) error {
	entry, ok := s.Entry(id)
	if !ok {
		return errdefs.NotFoundf("plugin store: plugin %q not found", id)
	}
	// Enabling a plugin whose tool namespace is taken would publish tools
	// that cannot resolve, so it is refused while the collision lasts.
	// Disabling it is the way out: the collision is gone as soon as one
	// of the two plugins stops claiming the namespace.
	if enabled && entry.Conflict != "" {
		return errdefs.Conflictf(
			"plugin store: plugin %q cannot be enabled: %s", id, entry.Conflict)
	}
	// The fail-closed rebuild of an unreadable state file needs the set
	// it must disable, and scan reads the same state file under mu, so
	// the set is gathered before the lock is taken.
	ids := s.validIDs()
	s.mu.Lock()
	state := s.stateForWriteLocked(ids)
	state[id] = enabled
	if err := s.saveStateLocked(state); err != nil {
		s.mu.Unlock()
		return err
	}
	callbacks := s.bumpLocked()
	s.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}
	return nil
}

// validIDs lists every valid scanned plugin id, in scan order.
func (s *Store) validIDs() []string {
	entries, _ := s.scan()
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Error == "" {
			ids = append(ids, entry.ID)
		}
	}
	return ids
}

// stateForWriteLocked is the state a write starts from: the recorded
// one when it reads, and the fail-closed view when it does not — every
// id the scan still sees, disabled. Starting from "no records" would
// write a file that re-enables everything the unreadable one held, the
// exact opposite of what the user was shown.
func (s *Store) stateForWriteLocked(ids []string) map[string]bool {
	state, err := s.loadStateLocked()
	if err == nil {
		return state
	}
	state = make(map[string]bool, len(ids))
	for _, id := range ids {
		state[id] = false
	}
	return state
}

// DataDir returns (creating it) the plugin's data directory.
func (s *Store) DataDir(id string) (string, error) {
	if err := ValidateID(id); err != nil {
		return "", err
	}
	dir := filepath.Join(s.opts.DataDirRoot, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errdefs.Validationf("plugin: create data dir: %w", err)
	}
	return dir, nil
}

// KV returns the plugin's namespaced key/value store.
func (s *Store) KV(id string) (*KV, error) {
	if err := ValidateID(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if store := s.kv[id]; store != nil {
		return store, nil
	}
	path := filepath.Join(s.opts.StateDir, "kv", id+".json")
	store, err := openKV(path)
	if err != nil {
		return nil, err
	}
	s.kv[id] = store
	return store, nil
}

// scan returns every plugin across the configured roots, in id order,
// plus the error of an unreadable enable state. A state file that does
// not exist is the fresh-install case and reads as "no records"; any
// other failure — a truncated or unreadable file — is returned and
// reported as every plugin disabled, so a broken file cannot re-enable
// plugins the user had switched off.
func (s *Store) scan() ([]Entry, error) {
	entries := make(map[string]Entry)
	for _, root := range s.opts.Roots {
		dirs, err := os.ReadDir(root.Path)
		if err != nil {
			continue
		}
		for _, dir := range dirs {
			if !dir.IsDir() {
				continue
			}
			if strings.HasPrefix(dir.Name(), ".") || strings.HasPrefix(dir.Name(), "_") {
				continue
			}
			dirPath := filepath.Join(root.Path, dir.Name())
			entry := s.loadEntry(dirPath, root.Builtin)
			if previous, ok := entries[entry.ID]; ok {
				if previous.Builtin && !entry.Builtin {
					entry.ShadowsBuiltin = true
					entry.BuiltinVersion = previous.Manifest.Version
				}
				if previous.ShadowsBuiltin {
					entry.ShadowsBuiltin = previous.ShadowsBuiltin
					entry.BuiltinVersion = previous.BuiltinVersion
				}
			}
			entries[entry.ID] = entry
		}
	}
	s.mu.Lock()
	state, stateErr := s.loadStateLocked()
	s.mu.Unlock()
	out := make([]Entry, 0, len(entries))
	for id, entry := range entries {
		if enabled, ok := state[id]; ok {
			entry.Enabled = enabled
		} else {
			entry.Enabled = stateErr == nil
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	markToolNamespaceCollisions(out)
	return out, stateErr
}

// markToolNamespaceCollisions names the collision of every valid plugin
// whose MCP tool namespace another plugin already claimed. Two ids can
// collapse into one namespace — ToolPrefix rewrites anything outside
// [a-z0-9_-], and an id may contain "." — and a second plugin publishing
// the same tool name loses the registry's duplicate race silently: the
// tool simply does not exist. The collision is reported as
// [Entry.Conflict] rather than as an invalid manifest, so the user sees
// which two plugins collide and can disable one of them: the colliding
// plugin cannot be enabled while the collision lasts, but it can be
// turned off.
func markToolNamespaceCollisions(entries []Entry) {
	owner := make(map[string]string, len(entries))
	for i, entry := range entries {
		if entry.Error != "" {
			continue
		}
		for _, namespace := range ToolNamespaces(entry.ID, entry.Manifest.Servers()) {
			if first, taken := owner[namespace]; taken {
				entries[i].Conflict = fmt.Sprintf(
					"the tool namespace %q collides with plugin %q",
					namespace, first)
				break
			}
			owner[namespace] = entry.ID
		}
	}
}

func (s *Store) loadEntry(dir string, builtin bool) Entry {
	entry := Entry{Dir: dir, Builtin: builtin}
	data, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if err != nil {
		entry.ID = filepath.Base(dir)
		entry.Error = "plugin.json: " + err.Error()
		return entry
	}
	manifest, err := ParseManifest(context.Background(), data)
	if err != nil {
		entry.ID = filepath.Base(dir)
		entry.Error = err.Error()
		return entry
	}
	entry.ID = manifest.ID
	entry.Manifest = manifest
	if s.opts.HostVersion != "" && manifest.MinHostVersion != "" &&
		version.Compare(s.opts.HostVersion, manifest.MinHostVersion) < 0 {
		entry.Error = "requires host version >= " + manifest.MinHostVersion
		return entry
	}
	if err := manifest.Validate(dir); err != nil {
		entry.Error = err.Error()
	}
	return entry
}

func (s *Store) statePath() string {
	return filepath.Join(s.opts.StateDir, "enabled.json")
}

// loadStateLocked reads the enable state. A file that does not exist is
// the fresh-install case and reads as no records — every plugin then
// takes the default. Any other failure is returned: a truncated or
// unreadable file must not read as "no records", which is what silently
// re-enables plugins the user had switched off.
func (s *Store) loadStateLocked() (map[string]bool, error) {
	data, err := os.ReadFile(s.statePath())
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, errdefs.Validationf(
			"plugin store: read %s: %w", s.statePath(), err)
	}
	state := map[string]bool{}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, errdefs.Validationf(
			"plugin store: parse %s: %w", s.statePath(), err)
	}
	return state, nil
}

func (s *Store) saveStateLocked(state map[string]bool) error {
	data, err := json.Marshal(state)
	if err != nil {
		return errdefs.Internal(err)
	}
	return writeFileAtomic(s.statePath(), data)
}

// KV is one plugin's namespaced key/value store.
type KV struct {
	mu     sync.Mutex
	path   string
	values map[string]string
}

// openKV reads one plugin's key/value file. A file that does not exist is
// a plugin that never wrote anything and reads as empty; anything else —
// unreadable, truncated, not JSON, not an object — is returned as an
// error. Reading a broken file as empty is not a lenient default: the
// next Set would write that empty table over the user's data.
func openKV(path string) (*KV, error) {
	store := &KV{path: path, values: map[string]string{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, errdefs.Validationf("plugin store: read %s: %w", path, err)
	}
	values := map[string]string{}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, errdefs.Validationf(
			"plugin store: %s is not a valid KV file: %w", path, err)
	}
	if values == nil {
		values = map[string]string{}
	}
	store.values = values
	return store, nil
}

// Get returns one value.
func (k *KV) Get(key string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	value, ok := k.values[key]
	return value, ok
}

// Set stores one value.
func (k *KV) Set(key, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.values[key] = value
	return k.persistLocked()
}

// Delete removes one value.
func (k *KV) Delete(key string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.values, key)
	return k.persistLocked()
}

// List returns a copy of every value.
func (k *KV) List() map[string]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make(map[string]string, len(k.values))
	for key, value := range k.values {
		out[key] = value
	}
	return out
}

func (k *KV) persistLocked() error {
	data, err := json.Marshal(k.values)
	if err != nil {
		return errdefs.Internal(err)
	}
	return writeFileAtomic(k.path, data)
}

// writeFileAtomic writes data through a temporary file and rename.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errdefs.Validationf("plugin store: create dir: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return errdefs.Validationf("plugin store: temp file: %w", err)
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return errdefs.Validationf("plugin store: write: %w", err)
	}
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return errdefs.Validationf("plugin store: chmod: %w", err)
	}
	if err := temp.Close(); err != nil {
		return errdefs.Validationf("plugin store: close: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		return errdefs.Validationf("plugin store: rename: %w", err)
	}
	return nil
}
