package deploy

import (
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// External declares one caller-owned dependency supplied to the
// deployment builder. Externals are visible to dependency resolution
// but are never constructed, wired, or closed by deploy.
type External struct {
	// Name is the resource-ref name used in document deps, e.g. "db".
	Name string `json:"name"`
	// Contract is the expected DepSpec.Type of consuming factories,
	// e.g. "db.Pool".
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
// value that satisfies it.
type ExternalResource struct {
	External
	Value any
}

// Validate checks the declaration and rejects nil values.
func (e ExternalResource) Validate() error {
	if err := e.External.Validate(); err != nil {
		return err
	}
	if e.Value == nil || isNilValue(e.Value) {
		return errdefs.Validationf(
			"deploy external %q: value is required", e.Name)
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
