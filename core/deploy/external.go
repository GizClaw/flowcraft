package deploy

import (
	"reflect"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

// External declares one caller-owned dependency supplied to the
// deployment builder. Externals are visible to dependency resolution
// but are never constructed, wired, or closed by deploy.
type External struct {
	// Name is the resource-ref name used in document deps, e.g. "db".
	Name string `json:"name"`
	// Contract is the author's declared contract name, e.g. "db.Pool". It is
	// document-facing: it travels with the deployment model so a reviewer can
	// read what an injected value is meant to be. The check the assembly
	// layer actually enforces is [ExternalResource.Type], derived from the
	// injected Go value, so a stale contract string can never admit a
	// mismatched value.
	Contract string `json:"contract"`
}

// Validate checks the static fields of an External declaration.
func (e External) Validate() error {
	e.Name = strings.TrimSpace(e.Name)
	e.Contract = strings.TrimSpace(e.Contract)
	if e.Name == "" || strings.Contains(e.Name, "/") {
		return errdefs.Validationf(
			"deploy external: name must be non-empty and must not contain '/'")
	}
	if e.Contract == "" {
		return errdefs.Validationf(
			"deploy external %q: contract is required", e.Name)
	}
	return nil
}

// ExternalResource pairs one External declaration with the caller-owned
// value that satisfies it, plus the value's Go type.
//
// Type is recorded at injection time and is what the plan checks a consuming
// factory's [resource.Requirement] against. It is the value's concrete type,
// so injecting a *tool.Registry satisfies a dependency that requires the
// tool.Source interface.
type ExternalResource struct {
	External
	Value any
	Type  reflect.Type
}

// NewExternal records one caller-owned dependency together with its Go type.
// Injection sites use this constructor rather than a struct literal so an
// external can never enter the builder without a type to check against.
func NewExternal(name, contract string, value any) ExternalResource {
	return ExternalResource{
		External: External{Name: strings.TrimSpace(name), Contract: strings.TrimSpace(contract)},
		Value:    value,
		Type:     reflect.TypeOf(value),
	}
}

// Validate checks the declaration and rejects nil values.
func (e ExternalResource) Validate() error {
	if err := e.External.Validate(); err != nil {
		return err
	}
	if e.Value == nil || ptr.IsNil(e.Value) {
		return errdefs.Validationf(
			"deploy external %q: value is required", e.Name)
	}
	if e.Type == nil {
		return errdefs.Validationf(
			"deploy external %q: inject the value with deploy.NewExternal so its "+
				"Go type is recorded", e.Name)
	}
	return nil
}

// WithExternalResources configures the builder with caller-owned
// dependency values. Values are copied by reference: deploy never
// closes or mutates them. Multiple calls accumulate; duplicates are
// rejected at Build time.
func WithExternalResources(externals []ExternalResource) BuilderOption {
	return func(b *Builder) {
		b.externalResources = append(b.externalResources, externals...)
	}
}

// normalizeExternalResources validates and deduplicates the builder's
// external values into a lookup map.
func normalizeExternalResources(
	externals []ExternalResource,
) (map[string]ExternalResource, error) {
	out := make(map[string]ExternalResource, len(externals))
	for _, ext := range externals {
		ext.Name = strings.TrimSpace(ext.Name)
		ext.Contract = strings.TrimSpace(ext.Contract)
		if err := ext.Validate(); err != nil {
			return nil, err
		}
		if _, dup := out[ext.Name]; dup {
			return nil, errdefs.Conflictf(
				"deploy external: duplicate dependency %q", ext.Name)
		}
		out[ext.Name] = ext
	}
	return out, nil
}
