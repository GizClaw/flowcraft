package runtime

import (
	"context"
	"fmt"
	"reflect"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
)

// DependencySpec declares one deployment resource an integration may borrow.
type DependencySpec struct {
	Name     string
	Kind     string
	Type     reflect.Type
	Required bool
}

// IntegrationSpec is the static catalog declaration for a factory kind.
type IntegrationSpec struct {
	Kind string
	Deps []DependencySpec
}

// IntegrationFactory prepares one configured integration before deploy.Build.
//
// Prepare must only allocate configuration-local state. It must not start
// goroutines or use deployment resources, which do not exist until Bind.
type IntegrationFactory interface {
	Spec() IntegrationSpec
	Prepare(context.Context, PrepareInput) (PreparedIntegration, error)
}

// PreparedIntegration participates in the remaining runtime lifecycle phases.
// Values received through Bind are borrowed and must never be closed.
type PreparedIntegration interface {
	Bind(context.Context, BindInput) error
	DecorateHost(session.HostFactory) (session.HostFactory, error)
	Start(context.Context) error
	Close() error
}

// BuildRollbacker is an optional compensation hook for state a prepared
// integration mutates while Builder is assembling a Runtime. Builder invokes
// it only when Build fails, before ordinary Close, and in reverse integration
// order. A successfully built Runtime never invokes Rollback.
type BuildRollbacker interface {
	Rollback(context.Context) error
}

// PrepareInput contains strictly decoded integration-local configuration.
type PrepareInput struct {
	Name     string
	Kind     string
	Settings *deploy.Opaque

	// Deploy is the borrowed deployment builder that will assemble the
	// application after Prepare returns. Integrations may register the
	// pre-build factories or sources their configuration needs, but must
	// not invoke Build or assume ownership of the builder.
	Deploy *deploy.Builder
}

// Deployment is the read-only part of a built deployment exposed to
// integrations. It deliberately omits Close and resource-map access.
type Deployment interface {
	Instance(id string) (*deploy.Instance, bool)
	InstanceNames() []string
}

// BindInput contains the integration's declared, validated borrowed
// resources and runtime capabilities. None of these values transfer
// lifecycle ownership to the integration.
type BindInput struct {
	Name         string
	Dependencies Dependencies
	Deployment   Deployment
	BaseHosts    session.HostFactory
}

// Dependencies is a read-only borrowed dependency view. It has no lifecycle
// methods; deployment Result remains the sole owner of all returned values.
type Dependencies struct {
	values map[string]any
}

// Get returns one borrowed dependency.
func (d Dependencies) Get(name string) (any, bool) {
	value, ok := d.values[name]
	return value, ok
}

// DependencyAs returns one borrowed dependency as T and rejects nil values.
func DependencyAs[T any](d Dependencies, name string) (T, error) {
	var zero T
	value, ok := d.Get(name)
	if !ok {
		return zero, errdefs.NotFoundf("runtime integration dependency %q is not bound", name)
	}
	if isNil(value) {
		return zero, errdefs.Internalf("runtime integration dependency %q is nil", name)
	}
	typed, ok := value.(T)
	if !ok || isNil(typed) {
		return zero, errdefs.Validationf(
			"runtime integration dependency %q has Go type %T, want %v",
			name, value, reflect.TypeFor[T]())
	}
	return typed, nil
}

func validateIntegrationSpec(spec IntegrationSpec) error {
	if spec.Kind == "" {
		return errdefs.Validationf("runtime integration factory: kind is required")
	}
	seen := make(map[string]struct{}, len(spec.Deps))
	for i, dep := range spec.Deps {
		if dep.Name == "" {
			return errdefs.Validationf(
				"runtime integration factory %q: deps[%d].name is required", spec.Kind, i)
		}
		if dep.Kind == "" {
			return errdefs.Validationf(
				"runtime integration factory %q: dep %q kind is required",
				spec.Kind, dep.Name)
		}
		if dep.Type == nil {
			return errdefs.Validationf(
				"runtime integration factory %q: dep %q Go type is required",
				spec.Kind, dep.Name)
		}
		if _, duplicate := seen[dep.Name]; duplicate {
			return errdefs.Validation(fmt.Errorf(
				"runtime integration factory %q: duplicate dep %q",
				spec.Kind, dep.Name))
		}
		seen[dep.Name] = struct{}{}
	}
	return nil
}

func cloneIntegrationSpec(spec IntegrationSpec) IntegrationSpec {
	spec.Deps = append([]DependencySpec(nil), spec.Deps...)
	return spec
}
