package config

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"sort"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Deps supplies application-side values that are not part of the
// document.
type Deps struct {
	// BaseDir resolves relative roots used by the local driver.
	BaseDir string
}

// Resource is the result of a workspace driver factory. Root is optional
// host root metadata and does not affect workspace path semantics.
type Resource struct {
	Workspace workspace.Workspace
	Root      string
	// Close optionally releases resources owned by the factory. When
	// omitted, Builder uses Workspace.Close if the concrete workspace
	// implements io.Closer. Scope decoration does not hide this
	// lifecycle hook.
	Close func() error
}

// Factory constructs one workspace from its driver-owned settings. A
// custom implementation decodes its own configuration from
// [sdkconfig.Input.Settings].
type Factory = sdkconfig.Func[sdkconfig.Input, Resource]

// Builder owns an instance-local driver catalog.
type Builder struct {
	deps    Deps
	catalog *sdkconfig.Registry[sdkconfig.Input, Resource]
}

// NewBuilder creates a builder with the local and memory drivers
// registered.
func NewBuilder(deps Deps) *Builder {
	b := &Builder{
		deps:    deps,
		catalog: sdkconfig.NewRegistry[sdkconfig.Input, Resource](),
	}
	b.registerBuiltins()
	return b
}

// RegisterFactory registers a custom driver. Duplicate names, empty
// names, and nil factories are validation errors.
func (b *Builder) RegisterFactory(driver string, factory Factory) error {
	if b == nil {
		return errdefs.Validationf("workspace config: builder is nil")
	}
	if err := b.catalog.Register(driver, factory); err != nil {
		return errdefs.Validationf("workspace config: %v", err)
	}
	return nil
}

// Build constructs all named workspaces and returns a read-only
// registry.
func (b *Builder) Build(ctx context.Context, doc Document) (_ *Registry, err error) {
	if b == nil {
		return nil, errdefs.Validationf("workspace config: builder is nil")
	}
	if ctx == nil {
		return nil, errdefs.Validationf("workspace config: context is nil")
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}

	resources := make(map[string]Resource, len(doc.Workspaces))
	var closers []func() error
	defer func() {
		if err != nil {
			err = joinCloseError(err, closeAll(closers))
		}
	}()
	for _, name := range sortedKeys(doc.Workspaces) {
		entry := doc.Workspaces[name]
		resource, err := b.catalog.Build(ctx, entry.Driver, sdkconfig.Input{
			Settings: entry.Settings,
		})
		if err != nil {
			if errdefs.IsNotFound(err) {
				return nil, errdefs.Validationf(
					"workspace config workspaces[%q]: unknown driver %q",
					name, entry.Driver)
			}
			return nil, asValidation(fmt.Errorf(
				"workspace config workspaces[%q] (%s): %w",
				name, entry.Driver, err))
		}
		if resource.Close != nil {
			closers = append(closers, resource.Close)
		}
		if isNilWorkspace(resource.Workspace) {
			return nil, errdefs.Validationf(
				"workspace config workspaces[%q] (%s): factory returned nil workspace",
				name, entry.Driver)
		}
		if resource.Close == nil {
			if closeFn := resourceCloser(resource); closeFn != nil {
				closers = append(closers, closeFn)
			}
		}
		if entry.Scope != nil {
			resource.Workspace = applyScope(resource.Workspace, *entry.Scope)
		}
		resources[name] = resource
	}
	return newRegistry(resources, closers), nil
}

func resourceCloser(resource Resource) func() error {
	if closer, ok := resource.Workspace.(io.Closer); ok {
		return closer.Close
	}
	return nil
}

func applyScope(inner workspace.Workspace, scope Scope) workspace.Workspace {
	return workspace.NewScopedWorkspace(inner,
		workspace.WithDenyRead(scope.DenyRead...),
		workspace.WithAllowWrite(scope.AllowWrite...),
		workspace.WithMandatoryDeny(scope.MandatoryDeny...),
	)
}

func isNilWorkspace(ws workspace.Workspace) bool {
	if ws == nil {
		return true
	}
	value := reflect.ValueOf(ws)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func asValidation(err error) error {
	err = errdefs.FromContext(err)
	if errdefs.HasClassification(err) {
		return err
	}
	return errdefs.Validation(err)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
