package resource

import (
	"context"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// SecretStore resolves one secret value by name. Implementations are
// pluggable backends (env, file, keychain, vault, ...); deployment
// resources of kind "secret.Store" are the declarative wiring, and any
// store can also be injected directly as a Scheme via
// [NewSecretScheme].
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

// NewSecretScheme builds the "secret" scheme over stores. References
// take one of two forms:
//
//	${secret:NAME}        — resolved through defaultStore
//	${secret:store.NAME}  — resolved through the named store
//
// The store prefix is everything before the first dot in the reference.
// A NAME-only reference with no default store is an error, as is an
// unknown store, an empty name, or a missing secret.
func NewSecretScheme(stores map[string]SecretStore, defaultStore string) Scheme {
	return SchemeFunc{
		SchemeName: "secret",
		Fn: func(ctx context.Context, ref string) (string, error) {
			storeName, name := splitSecretRef(ref, defaultStore)
			if storeName == "" {
				return "", errdefs.Validationf(
					"resource settings expand: secret reference ${secret:%s} needs an explicit store or a default secret.Store",
					ref)
			}
			store, ok := stores[storeName]
			if !ok {
				return "", errdefs.Validationf(
					"resource settings expand: secret store %q is not configured", storeName)
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return "", errdefs.Validationf(
					"resource settings expand: secret name is empty")
			}
			value, found, err := store.Lookup(ctx, name)
			if err != nil {
				return "", errdefs.Validationf(
					"resource settings expand: secret store %q: %v", storeName, err)
			}
			if !found {
				return "", errdefs.Validationf(
					"resource settings expand: secret %q not found in store %q",
					name, storeName)
			}
			return value, nil
		},
	}
}

// splitSecretRef parses "store.NAME" or "NAME" (default store).
func splitSecretRef(ref, defaultStore string) (storeName, name string) {
	storeName = defaultStore
	name = ref
	if before, after, ok := strings.Cut(ref, "."); ok && strings.TrimSpace(before) != "" {
		storeName = before
		name = after
	}
	return storeName, name
}
