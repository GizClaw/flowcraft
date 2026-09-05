package runtime

import (
	"strings"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// ExternalDependency declares one caller-owned dependency in the
// runtime section of a deployment document.
type ExternalDependency struct {
	// Name is the resource-ref name used in document deps, e.g. "db".
	Name string `json:"name"`
	// Contract is the expected DepSpec.Type of consuming factories,
	// e.g. "db.Pool".
	Contract string `json:"contract"`
}

// Validate checks one external dependency declaration.
func (d ExternalDependency) Validate() error {
	d.Name = strings.TrimSpace(d.Name)
	d.Contract = strings.TrimSpace(d.Contract)
	if d.Name == "" || strings.Contains(d.Name, "/") {
		return errdefs.Validationf(
			"runtime external_deps: name must be non-empty and must not contain '/'")
	}
	if d.Contract == "" {
		return errdefs.Validationf(
			"runtime external_deps: %q contract is required", d.Name)
	}
	return nil
}

// ExternalResource pairs one external dependency declaration with the
// caller-owned value that satisfies it.
type ExternalResource struct {
	ExternalDependency
	Value any
}

// Validate checks the declaration and rejects nil values.
func (e ExternalResource) Validate() error {
	if err := e.ExternalDependency.Validate(); err != nil {
		return err
	}
	if e.Value == nil || isNil(e.Value) {
		return errdefs.Validationf(
			"runtime external_deps: %q value is required", e.Name)
	}
	return nil
}

func toDeployExternalResource(e ExternalResource) deploy.ExternalResource {
	return deploy.ExternalResource{
		External: deploy.External{
			Name:     strings.TrimSpace(e.Name),
			Contract: strings.TrimSpace(e.Contract),
		},
		Value: e.Value,
	}
}

// validateExternalDependencyList validates declarations and rejects
// duplicates.
func validateExternalDependencyList(
	deps []ExternalDependency,
) (map[string]ExternalDependency, error) {
	out := make(map[string]ExternalDependency, len(deps))
	for _, dep := range deps {
		dep.Name = strings.TrimSpace(dep.Name)
		dep.Contract = strings.TrimSpace(dep.Contract)
		if err := dep.Validate(); err != nil {
			return nil, err
		}
		if _, dup := out[dep.Name]; dup {
			return nil, errdefs.Conflictf(
				"runtime external_deps: duplicate dependency %q", dep.Name)
		}
		out[dep.Name] = dep
	}
	return out, nil
}

// validateExternalResourceList validates injected values and rejects
// duplicates.
func validateExternalResourceList(
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
				"runtime external_deps: duplicate injected resource %q", ext.Name)
		}
		out[ext.Name] = ext
	}
	return out, nil
}

// effectiveExternalResources validates that every declaration is
// covered by an injected value. injected is the set of available
// injected resources; values not declared are allowed when allowExtra
// is true (Reload may shrink the declaration set).
func effectiveExternalResources(
	declared []ExternalDependency,
	injected map[string]ExternalResource,
	allowExtra bool,
) ([]ExternalResource, error) {
	declaredMap, err := validateExternalDependencyList(declared)
	if err != nil {
		return nil, err
	}
	if !allowExtra {
		for name := range injected {
			if _, ok := declaredMap[name]; !ok {
				return nil, errdefs.Validationf(
					"runtime external_deps: injected resource %q is not declared in runtime config",
					name)
			}
		}
	}
	out := make([]ExternalResource, 0, len(declaredMap))
	for name, dep := range declaredMap {
		ext, ok := injected[name]
		if !ok {
			return nil, errdefs.Validationf(
				"runtime external_deps: %q is declared but no external resource was injected",
				name)
		}
		if ext.Contract != dep.Contract {
			return nil, errdefs.Validationf(
				"runtime external_deps: %q contract %q does not match declared contract %q",
				name, ext.Contract, dep.Contract)
		}
		out = append(out, ext)
	}
	return out, nil
}
