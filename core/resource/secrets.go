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

// GoString redacts the value for %#v too: without it, fmt would print
// the struct field by field, literal included, so a value hidden by
// every other verb would leak through the debug one.
func (s Secret) GoString() string { return "<secret>" }

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
		Fn: func(_ context.Context, ref Reference) (any, error) {
			path := ref.Path
			storeName, name := r.resolveRef(ref)
			if storeName == "" {
				return nil, errdefs.Validationf(
					"resource settings expand: secret reference ${secret:%s} needs an explicit store or a default secret.Store",
					path)
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

// resolveRef parses "store.NAME" or "NAME" (default store). A dotted
// ref whose prefix names a configured store is an explicit
// store.NAME reference; otherwise the whole ref is the secret name in
// the default store, so dotted names (docker/k8s file secrets like
// "my.app.key") stay addressable via NAME-only refs.
func (r *SecretResolver) resolveRef(ref Reference) (storeName, name string) {
	if before, after, ok := strings.Cut(ref.Path, "."); ok && strings.TrimSpace(before) != "" {
		if _, exists := r.stores[before]; exists {
			return before, after
		}
	}
	return r.defaultStore, ref.Path
}

type secretCacheEntry struct {
	value   string
	found   bool
	expires time.Time
}

// defaultSecretCacheCapacity bounds a caching store's entries. Secret
// names are data (a deployment can resolve one per tenant, request, or
// file key), so the cache has to be bounded like every other core cache
// — see event.WithRouteCacheSize for the same reasoning.
const defaultSecretCacheCapacity = 256

// CachingSecretStore wraps a SecretStore with an in-memory TTL cache so
// per-request lazy resolution does not hit the backend every time.
//
// The TTL bounds freshness, not retention: an expired entry is dropped
// when its own name is looked up again, and every TTL window sweeps the
// rest, so a name that is never looked up again cannot keep its value
// (or a stale negative result) alive for more than one extra window.
// The capacity bound — see [WithCacheCapacity] — caps how many names are
// held at once, whatever the TTL.
type CachingSecretStore struct {
	inner SecretStore
	ttl   time.Duration

	capacity int
	// reapAt is when the next expired-entry sweep is due. Zero means
	// "sweep on the next store".
	reapAt time.Time

	mu    sync.Mutex
	cache map[string]secretCacheEntry
}

// CachingSecretStoreOption configures a [CachingSecretStore] at
// construction time.
type CachingSecretStoreOption func(*CachingSecretStore)

// WithCacheCapacity bounds how many lookups the cache holds at once,
// counting values and negative results alike. n <= 0 keeps the default
// ([defaultSecretCacheCapacity]). When the bound is reached the entry
// closest to expiry is evicted first, so long-lived names keep their
// place ahead of ones about to lapse.
func WithCacheCapacity(n int) CachingSecretStoreOption {
	return func(c *CachingSecretStore) {
		if n > 0 {
			c.capacity = n
		}
	}
}

// NewCachingSecretStore returns a caching wrapper. ttl <= 0 disables
// caching and delegates every lookup to inner.
func NewCachingSecretStore(
	inner SecretStore,
	ttl time.Duration,
	opts ...CachingSecretStoreOption,
) *CachingSecretStore {
	c := &CachingSecretStore{inner: inner, ttl: ttl}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Lookup implements SecretStore.
func (c *CachingSecretStore) Lookup(ctx context.Context, name string) (string, bool, error) {
	if c == nil || c.inner == nil {
		return "", false, errdefs.Validationf(
			"resource secret: caching store has no inner store")
	}
	if c.ttl <= 0 {
		return c.inner.Lookup(ctx, name)
	}
	now := time.Now()
	if entry, ok := c.cached(name, now); ok {
		return entry.value, entry.found, nil
	}

	value, found, err := c.inner.Lookup(ctx, name)
	if err != nil {
		return "", false, err
	}
	c.store(name, secretCacheEntry{
		value:   value,
		found:   found,
		expires: now.Add(c.ttl),
	}, now)
	return value, found, nil
}

// cached returns the live entry for name, dropping it first when it has
// expired.
func (c *CachingSecretStore) cached(name string, now time.Time) (secretCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[name]
	if !ok {
		return secretCacheEntry{}, false
	}
	if entry.expires.After(now) {
		return entry, true
	}
	delete(c.cache, name)
	return secretCacheEntry{}, false
}

// store records one lookup, reclaiming expired entries and evicting the
// entry closest to expiry once the capacity bound is reached.
func (c *CachingSecretStore) store(name string, entry secretCacheEntry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = make(map[string]secretCacheEntry, c.limit())
	}
	// Sweep on a timer rather than on every store: one pass per TTL
	// window keeps expired entries from accumulating without putting a
	// full scan on the per-lookup path. A lookup of the same name
	// reclaims its own stale entry immediately (see cached).
	if c.reapAt.IsZero() || !now.Before(c.reapAt) {
		c.reapExpired(now)
		c.reapAt = now.Add(c.ttl)
	}
	if _, exists := c.cache[name]; !exists && len(c.cache) >= c.limit() {
		c.evictSoonest()
	}
	c.cache[name] = entry
}

func (c *CachingSecretStore) limit() int {
	if c.capacity > 0 {
		return c.capacity
	}
	return defaultSecretCacheCapacity
}

// reapExpired drops every entry whose TTL has passed. Caller holds mu.
func (c *CachingSecretStore) reapExpired(now time.Time) {
	for name, entry := range c.cache {
		if !entry.expires.After(now) {
			delete(c.cache, name)
		}
	}
}

// evictSoonest drops the entry with the earliest expiry — which is an
// already-expired one whenever the cache holds any, so a stale entry is
// always the first to go. Caller holds mu and the cache is non-empty.
func (c *CachingSecretStore) evictSoonest() {
	var (
		victim  string
		soonest time.Time
		found   bool
	)
	for name, entry := range c.cache {
		if !found || entry.expires.Before(soonest) {
			victim, soonest, found = name, entry.expires, true
		}
	}
	if found {
		delete(c.cache, victim)
	}
}

// DefaultSecretStore implements SecretStore.
func (c *CachingSecretStore) DefaultSecretStore() bool {
	if c == nil || c.inner == nil {
		return false
	}
	return c.inner.DefaultSecretStore()
}
