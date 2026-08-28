// Package secret provides the "secret.Store" resource kind: pluggable
// credential backends that the deploy builder assembles into the
// ${secret:...} reference scheme. Implementations are registered as
// ordinary resource factories, so external backends (keychain, vault,
// 1Password, k8s, ...) plug in with zero core changes.
package secret

import (
	"context"

	"github.com/GizClaw/flowcraft/core/resource"
)

// ResourceKind is the deployment resource kind of a secret store.
const ResourceKind resource.Kind = "secret.Store"

// Store is a built secret.Store resource value: a named lookup backend.
// Resource identity comes from the deployment resource name; the
// default flag comes from the store's settings.
type Store struct {
	id     string
	def    bool
	lookup func(context.Context, string) (string, bool, error)
}

// Lookup implements resource.SecretStore.
func (s Store) Lookup(ctx context.Context, name string) (string, bool, error) {
	return s.lookup(ctx, name)
}

// DefaultSecretStore implements resource.SecretStore.
func (s Store) DefaultSecretStore() bool { return s.def }

// SecretStoreID implements resource.SecretStoreID. Empty falls back to
// the deployment resource name.
func (s Store) SecretStoreID() string { return s.id }
