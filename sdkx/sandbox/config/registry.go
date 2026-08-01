package config

import (
	"context"
	"errors"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	coresandbox "github.com/GizClaw/flowcraft/sdk/sandbox"
)

// Registry is an immutable catalog of built sandbox runners.
type Registry struct {
	runners map[string]coresandbox.Runner
	names   []string
	closers []func() error

	closeOnce sync.Once
	closeErr  error
}

func newRegistry(runners map[string]coresandbox.Runner, closers []func() error) *Registry {
	registry := &Registry{
		runners: make(map[string]coresandbox.Runner, len(runners)),
		names:   sortedKeys(runners),
		closers: append([]func() error(nil), closers...),
	}
	for name, runner := range runners {
		registry.runners[name] = runner
	}
	return registry
}

// Close releases closeable backend runners in reverse build order. It is
// idempotent; decorator wrapping does not hide backend lifecycle hooks.
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

// Get returns a named sandbox runner.
func (r *Registry) Get(name string) (coresandbox.Runner, bool) {
	if r == nil {
		return nil, false
	}
	runner, ok := r.runners[name]
	return runner, ok
}

// Resolve adapts the registry to sdkx/deploy.SourceFunc for the case
// where the HOST built this registry and keeps its lifetime. Register
// it directly:
//
//	deployBuilder.RegisterSource("sandbox", sandboxes.Resolve)
//
// A registry the deployment document should own and close belongs in
// the resource area instead — see [DeployResource].
func (r *Registry) Resolve(_ context.Context, ref string) (any, error) {
	runner, ok := r.Get(ref)
	if !ok {
		return nil, errdefs.Validationf(
			"sandbox config: unknown sandbox %q", ref)
	}
	return runner, nil
}

// Lookup implements sdkx/deploy.RefLookup so a deployment document can
// bind one runner out of this registry with the scalar dep form
// ("sandboxes/coding"). It returns the sandbox.Runner as any; Get is
// the typed accessor.
func (r *Registry) Lookup(ref string) (any, bool) {
	runner, ok := r.Get(ref)
	if !ok {
		return nil, false
	}
	return runner, true
}

// Names returns sorted sandbox names in a caller-owned slice.
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
