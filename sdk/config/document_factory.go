package config

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// DocumentFactory is a [ResourceFactory] whose settings subtree is a
// module-owned document — literal content, {file}, or {embed}. It
// provides the shared "decode settings as [Source], resolve to bytes
// through [Input.ResolveDocument], hand the bytes to the module's
// build step" flow, so a resource impl only declares its spec and its
// build step instead of a bespoke deploy factory.
//
// The build step receives the resolved document bytes and the bound
// dependencies (keyed by the spec's dep names) and returns the built
// resource value; it may type-assert dependencies and must return
// errdefs-classified errors.
type DocumentFactory struct {
	spec  ResourceSpec
	build func(ctx context.Context, data []byte, deps map[string]any) (any, error)
}

// NewDocumentFactory returns a [ResourceFactory] that resolves its
// settings document through the input's resolver and passes the bytes
// to build. The spec is validated when the factory is registered on a
// deployment builder; build must be non-nil.
func NewDocumentFactory(
	spec ResourceSpec,
	build func(ctx context.Context, data []byte, deps map[string]any) (any, error),
) ResourceFactory {
	return &DocumentFactory{spec: spec.Clone(), build: build}
}

// Spec implements [ResourceFactory].
func (f *DocumentFactory) Spec() ResourceSpec {
	return f.spec.Clone()
}

// New implements [ResourceFactory].
func (f *DocumentFactory) New(ctx context.Context, in Input) (any, error) {
	if f.build == nil {
		return nil, errdefs.Validationf(
			"config: document factory has no build step")
	}
	data, err := in.ResolveDocument(ctx)
	if err != nil {
		return nil, err
	}
	return f.build(ctx, data, in.Deps)
}
