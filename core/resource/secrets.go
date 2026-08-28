package resource

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// SecretStore resolves one secret value by name. Implementations are
// pluggable backends (env, file, keychain, vault, ...); deployment
// resources of kind "secret.Store" are the declarative wiring.
type SecretStore interface {
	// Lookup returns the secret named name. found=false means the
	// store has no such secret; other errors are store failures.
	Lookup(ctx context.Context, name string) (value string, found bool, err error)
	// DefaultSecretStore reports whether NAME-less ${secret:...}
	// references may resolve through this store. At most one store in
	// a deployment may report true.
	DefaultSecretStore() bool
}

// SecretStoreID is optionally implemented by secret store values to
// give the store a shorter reference id than its deployment resource
// name. When absent, the resource name is used in ${secret:store.NAME}
// references.
type SecretStoreID interface {
	SecretStoreID() string
}

// SecretStoreFunc adapts a lookup function to [SecretStore].
type SecretStoreFunc struct {
	LookupFn      func(context.Context, string) (string, bool, error)
	DefaultSecret bool
}

func (f SecretStoreFunc) Lookup(ctx context.Context, name string) (string, bool, error) {
	return f.LookupFn(ctx, name)
}

func (f SecretStoreFunc) DefaultSecretStore() bool { return f.DefaultSecret }

// SecretRef is the wire shape of a lazy secret reference produced by
// the "secret" scheme and decoded by [Secret].
type SecretRef struct {
	Store string `json:"store"`
	Name  string `json:"name"`
}

// Secret is a settings value that never renders its contents: either a
// literal string or a lazy reference resolved through a
// [SecretResolver] at use time. String() and MarshalJSON always yield
// "<secret>", so secrets never leak through logs, errors, or
// snapshots.
type Secret struct {
	literal string
	ref     *SecretRef
}

// LiteralSecret wraps a plain value as a non-referencing Secret.
func LiteralSecret(value string) Secret { return Secret{literal: value} }

// IsRef reports whether s is a lazy reference rather than a literal.
func (s Secret) IsRef() bool { return s.ref != nil }

// Resolve returns the secret value. Literal secrets return their value
// directly; references are looked up through resolver at use time.
func (s Secret) Resolve(ctx context.Context, resolver *SecretResolver) (string, error) {
	if s.ref == nil {
		return s.literal, nil
	}
	if resolver == nil {
		return "", errdefs.Validationf(
			"resource secret: no resolver for %q", s.ref.Name)
	}
	return resolver.Resolve(ctx, s.ref.Store, s.ref.Name)
}

func (s Secret) String() string { return "<secret>" }

// MarshalJSON redacts the value.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"<secret>"`), nil
}

// UnmarshalJSON accepts a plain string (literal) or a SecretRef object
// (lazy reference).
func (s *Secret) UnmarshalJSON(data []byte) error {
	var literal string
	if err := json.Unmarshal(data, &literal); err == nil {
		s.literal = literal
		s.ref = nil
		return nil
	}
	var ref SecretRef
	if err := json.Unmarshal(data, &ref); err != nil {
		return errdefs.Validationf(
			"resource secret: want string or {\"store\":...,\"name\":...}, got %s", data)
	}
	if strings.TrimSpace(ref.Store) == "" || strings.TrimSpace(ref.Name) == "" {
		return errdefs.Validationf(
			"resource secret: reference must carry store and name")
	}
	s.ref = &ref
	s.literal = ""
	return nil
}

// SecretResolver maps store ids to [SecretStore] backends and resolves
// ${secret:...} references. The deploy builder assembles it from the
// declared secret.Store resources and hands it to factories via
// [Input].Secrets so typed [Secret] values resolve lazily at use time.
type SecretResolver struct {
	stores       map[string]SecretStore
	defaultStore string
}

// NewSecretResolver returns a resolver over stores. defaultStore is the
// target of NAME-only references; empty means NAME-only refs are
// rejected.
func NewSecretResolver(stores map[string]SecretStore, defaultStore string) *SecretResolver {
	return &SecretResolver{stores: stores, defaultStore: defaultStore}
}

// Scheme returns the "secret" reference scheme. ${secret:NAME} resolves
// through the default store; ${secret:store.NAME} through the named
// store. Resolution emits a [SecretRef] marker instead of the value, so
// the actual lookup is deferred to [Secret.Resolve] — deployments get
// lazy, request-time resolution with build-time validation of the
// store and name.
func (r *SecretResolver) Scheme() Scheme {
	return SchemeFunc{
		SchemeName: "secret",
		Fn: func(_ context.Context, ref string) (any, error) {
			storeName, name := r.split(ref)
			if storeName == "" {
				return nil, errdefs.Validationf(
					"resource settings expand: secret reference ${secret:%s} needs an explicit store or a default secret.Store",
					ref)
			}
			if _, ok := r.stores[storeName]; !ok {
				return nil, errdefs.Validationf(
					"resource settings expand: secret store %q is not configured", storeName)
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return nil, errdefs.Validationf(
					"resource settings expand: secret name is empty")
			}
			return SecretRef{Store: storeName, Name: name}, nil
		},
	}
}

// Resolve materializes one secret through its store.
func (r *SecretResolver) Resolve(ctx context.Context, storeName, name string) (string, error) {
	if r == nil {
		return "", errdefs.Validationf(
			"resource secret: no resolver for %q", name)
	}
	store, ok := r.stores[storeName]
	if !ok {
		return "", errdefs.Validationf(
			"resource secret: store %q is not configured", storeName)
	}
	value, found, err := store.Lookup(ctx, name)
	if err != nil {
		return "", errdefs.Validationf(
			"resource secret: store %q: %v", storeName, err)
	}
	if !found {
		return "", errdefs.Validationf(
			"resource secret: %q not found in store %q", name, storeName)
	}
	return value, nil
}

// split parses "store.NAME" or "NAME" (default store).
func (r *SecretResolver) split(ref string) (storeName, name string) {
	storeName = r.defaultStore
	name = ref
	if before, after, ok := strings.Cut(ref, "."); ok && strings.TrimSpace(before) != "" {
		storeName = before
		name = after
	}
	return storeName, name
}

type secretCacheEntry struct {
	value   string
	found   bool
	expires time.Time
}

// CachingSecretStore wraps a SecretStore with an in-memory TTL cache so
// per-request lazy resolution does not hit the backend every time.
type CachingSecretStore struct {
	inner SecretStore
	ttl   time.Duration

	mu    sync.Mutex
	cache map[string]secretCacheEntry
}

// NewCachingSecretStore returns a caching wrapper. ttl <= 0 disables
// caching and delegates every lookup to inner.
func NewCachingSecretStore(inner SecretStore, ttl time.Duration) *CachingSecretStore {
	return &CachingSecretStore{inner: inner, ttl: ttl}
}

// Lookup implements SecretStore.
func (c *CachingSecretStore) Lookup(ctx context.Context, name string) (string, bool, error) {
	if c == nil || c.inner == nil || c.ttl <= 0 {
		if c == nil || c.inner == nil {
			return "", false, nil
		}
		return c.inner.Lookup(ctx, name)
	}
	now := time.Now()
	c.mu.Lock()
	if entry, ok := c.cache[name]; ok && entry.expires.After(now) {
		c.mu.Unlock()
		return entry.value, entry.found, nil
	}
	c.mu.Unlock()

	value, found, err := c.inner.Lookup(ctx, name)
	if err != nil {
		return "", false, err
	}
	c.mu.Lock()
	if c.cache == nil {
		c.cache = make(map[string]secretCacheEntry)
	}
	c.cache[name] = secretCacheEntry{value: value, found: found, expires: now.Add(c.ttl)}
	c.mu.Unlock()
	return value, found, nil
}

// DefaultSecretStore implements SecretStore.
func (c *CachingSecretStore) DefaultSecretStore() bool {
	if c == nil || c.inner == nil {
		return false
	}
	return c.inner.DefaultSecretStore()
}
