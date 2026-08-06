package config

import (
	"context"
	"errors"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

// Registry is an immutable catalog of built workspace resources.
type Registry struct {
	workspaces map[string]workspace.Workspace
	roots      map[string]string
	names      []string
	closers    []func() error
	closeOnce  sync.Once
	closeErr   error
}

func newRegistry(resources map[string]Resource, closers []func() error) *Registry {
	registry := &Registry{
		workspaces: make(map[string]workspace.Workspace, len(resources)),
		roots:      make(map[string]string, len(resources)),
		names:      sortedKeys(resources),
		closers:    append([]func() error(nil), closers...),
	}
	for name, resource := range resources {
		registry.workspaces[name] = resource.Workspace
		if resource.Root != "" {
			registry.roots[name] = resource.Root
		}
	}
	return registry
}

// Close releases factory-owned workspace resources in reverse build
// order. It is idempotent. Built-in local and memory workspaces need no
// cleanup.
func (r *Registry) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.closeErr = closeAll(r.closers)
		r.closers = nil
	})
	return r.closeErr
}

// Get returns the named workspace.
func (r *Registry) Get(name string) (workspace.Workspace, bool) {
	if r == nil {
		return nil, false
	}
	ws, ok := r.workspaces[name]
	return ws, ok
}

// Resolve adapts the registry to sdk/config.SourceFunc for the case
// where the HOST built this registry and keeps its lifetime. Register
// it directly:
//
//	builder.RegisterSource("workspace", workspaces.Resolve)
//
// A registry the deployment document should own and close belongs in
// the resource area instead — see [NewDeployFactory].
func (r *Registry) Resolve(_ context.Context, ref string) (any, error) {
	ws, ok := r.Get(ref)
	if !ok {
		return nil, errdefs.Validationf(
			"workspace config: unknown workspace %q", ref)
	}
	return ws, nil
}

// ResolveItem implements sdk/config.ItemResolver so a deployment
// document can bind one workspace out of this registry with the scalar
// dep form ("workspaces/project"). It returns the workspace.Workspace
// as any; Get is the typed accessor.
func (r *Registry) ResolveItem(ref string) (any, bool) {
	ws, ok := r.Get(ref)
	if !ok {
		return nil, false
	}
	return ws, true
}

// Root returns optional host-root metadata for a workspace.
func (r *Registry) Root(name string) (string, bool) {
	if r == nil {
		return "", false
	}
	root, ok := r.roots[name]
	return root, ok
}

// Names returns the sorted workspace names in a caller-owned slice.
func (r *Registry) Names() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.names...)
}

func closeAll(closers []func() error) error {
	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func joinCloseError(primary, closeErr error) error {
	if closeErr == nil {
		return primary
	}
	return errors.Join(primary, closeErr)
}
