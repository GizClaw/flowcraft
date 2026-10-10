package keychain

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/secret"
)

// ResourceImpl is the deployment impl id of the keychain secret store.
// It keeps the name "keychain" so deployment documents and
// ${secret:keychain.NAME} references keep their meaning.
const ResourceImpl = "keychain"

// factory builds the secret.Store/keychain resource.
type factory struct{}

// NewFactory returns a deployment resource factory for [Store].
func NewFactory() resource.Factory { return factory{} }

// Spec implements resource.Factory.
func (factory) Spec() resource.Spec {
	return resource.Spec{Kind: secret.ResourceKind, Impl: ResourceImpl}
}

// New implements resource.Factory.
func (factory) New(ctx context.Context, in resource.Input) (any, error) {
	settings, err := resource.DecodeTyped[Settings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validationf(
			"secret keychain config: decode settings: %w", err)
	}
	store, err := NewStore(settings.Dir)
	if err != nil {
		return nil, err
	}
	store.id = settings.ID
	store.def = settings.Default
	return store, nil
}

// Register adds the secret.Store/keychain factory to r.
func Register(r *resource.Registry) error {
	return r.Register(NewFactory())
}
