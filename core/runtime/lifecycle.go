package runtime

import (
	"context"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"

	otellog "go.opentelemetry.io/otel/log"
)

type registerOptions struct {
	toolResource string
}

// RegisterAgentOption configures one dynamic agent registration.
type RegisterAgentOption func(*registerOptions) error

// WithToolAssembly names a built tool.Assembly resource used as the new
// agent's dynamic catalog entry. It requires the deployment to have a
// dynamic_catalog section.
func WithToolAssembly(resourceName string) RegisterAgentOption {
	return func(o *registerOptions) error {
		if strings.TrimSpace(resourceName) == "" {
			return errdefs.Validationf(
				"runtime: WithToolAssembly requires a resource name")
		}
		o.toolResource = resourceName
		return nil
	}
}

type removeOptions struct {
	timeout time.Duration
}

// UnregisterAgentOption configures one dynamic agent removal.
type UnregisterAgentOption func(*removeOptions) error

// WithRemoveTimeout bounds how long UnregisterAgent waits for active
// turns to finish before giving up. On timeout the agent is left in
// place (registration intact, sessions intact) and the call is retryable.
func WithRemoveTimeout(d time.Duration) UnregisterAgentOption {
	return func(o *removeOptions) error {
		if d <= 0 {
			return errdefs.Validationf(
				"runtime: WithRemoveTimeout must be positive")
		}
		o.timeout = d
		return nil
	}
}

// RegisterAgent assembles and registers a new agent at runtime: the
// Definition is run through the same assembly path as deployment
// (deploy.BindAgent), then the agent becomes resolvable by the session
// manager. The name must not collide with an existing dynamic agent or
// a deployed agent (both are Conflict).
//
// RegisterAgent is serialized with UnregisterAgent and Close. After
// Close it fails with NotAvailable.
func (r *Runtime) RegisterAgent(
	ctx context.Context,
	name string,
	def agent.Definition,
	opts ...RegisterAgentOption,
) (*agent.Agent, error) {
	if r == nil {
		return nil, errdefs.Validationf(
			"runtime: RegisterAgent requires a built Runtime")
	}
	if isNilContext(ctx) {
		return nil, errdefs.Validationf(
			"runtime: RegisterAgent context is required")
	}
	if err := validateAgentID(name); err != nil {
		return nil, err
	}
	if err := def.Validate(); err != nil {
		return nil, errdefs.Validationf(
			"runtime: register agent %q: %v", name, err)
	}

	options := registerOptions{}
	for _, option := range opts {
		if isNil(option) {
			return nil, errdefs.Validationf(
				"runtime: RegisterAgentOption must not be nil")
		}
		if err := option(&options); err != nil {
			return nil, err
		}
	}

	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.closed {
		return nil, errdefs.NotAvailablef("runtime: closed")
	}
	if _, ok := r.registry.Agent(name); ok {
		return nil, errdefs.Conflictf("runtime: agent %q is already registered", name)
	}
	if _, deployed := r.result.Agent(name); deployed {
		return nil, errdefs.Conflictf(
			"runtime: agent %q is a deployed agent", name)
	}

	instance, err := deploy.BindAgent(ctx, r.resources, r.result, r.loader, nil, nil, name, def)
	if err != nil {
		return nil, err
	}
	closeInstance := func() {
		if cerr := instance.Close(); cerr != nil {
			telemetry.WarnErr(ctx, "runtime: close agent after registration failure", cerr,
				otellog.String(telemetry.AttrAgentID, name))
		}
	}

	var assembly *tool.Assembly
	if options.toolResource != "" {
		if r.liveCatalog == nil {
			closeInstance()
			return nil, errdefs.Validationf(
				"runtime: dynamic catalog is not configured; cannot attach tool assembly %q",
				options.toolResource)
		}
		value, ok := r.result.Value(options.toolResource)
		if !ok {
			closeInstance()
			return nil, errdefs.NotFoundf(
				"runtime: tool assembly resource %q not found",
				options.toolResource)
		}
		var typeOK bool
		assembly, typeOK = value.(*tool.Assembly)
		if !typeOK || assembly == nil {
			closeInstance()
			return nil, errdefs.Validationf(
				"runtime: tool assembly resource %q is %T, want *tool.Assembly",
				options.toolResource, value)
		}
	} else if r.liveCatalog != nil && !r.liveCatalog.hasDefault() {
		// Mirrors the build-time rule: with a dynamic catalog every
		// agent must have an explicit tool assembly or a default.
		closeInstance()
		return nil, errdefs.Validationf(
			"runtime: dynamic catalog has no default; agent %q needs WithToolAssembly",
			name)
	}

	r.manager.ReopenAgent(name)
	if r.liveCatalog != nil {
		r.liveCatalog.Set(name, assembly)
	}
	if err := r.registry.Put(name, dynamicAgentEntry{
		instance:     instance,
		definition:   def,
		toolAssembly: options.toolResource,
	}); err != nil {
		closeInstance()
		return nil, err
	}
	r.publishLifecycleEvent(ctx, SubjectAgentRegistered(name), AgentLifecycleEvent{
		AgentID:     name,
		Name:        def.Card.Name,
		Description: def.Card.Description,
	})
	telemetry.Info(ctx, "runtime agent registered",
		otellog.String(telemetry.AttrAgentID, name),
		otellog.String("agent.card.name", def.Card.Name))
	return instance, nil
}

// UnregisterAgent removes a dynamically registered agent: new session
// activity is blocked, live sessions are drained (active turns are
// allowed to finish, bounded by ctx or WithRemoveTimeout), and the
// agent's engine and hooks are closed. Unknown names are an idempotent
// no-op; deployed (static) agents cannot be removed at runtime.
func (r *Runtime) UnregisterAgent(
	ctx context.Context,
	name string,
	opts ...UnregisterAgentOption,
) error {
	if r == nil {
		return errdefs.Validationf(
			"runtime: UnregisterAgent requires a built Runtime")
	}
	if isNilContext(ctx) {
		return errdefs.Validationf(
			"runtime: UnregisterAgent context is required")
	}
	if err := validateAgentID(name); err != nil {
		return err
	}

	options := removeOptions{}
	for _, option := range opts {
		if isNil(option) {
			return errdefs.Validationf(
				"runtime: UnregisterAgentOption must not be nil")
		}
		if err := option(&options); err != nil {
			return err
		}
	}

	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.closed {
		return errdefs.NotAvailablef("runtime: closed")
	}

	entry, ok := r.registry.Delete(name)
	if !ok {
		if _, deployed := r.result.Agent(name); deployed {
			return errdefs.Conflictf(
				"runtime: agent %q is a deployed agent; remove it from the deployment document",
				name)
		}
		return nil // idempotent: unknown name
	}

	removeCtx := ctx
	if options.timeout > 0 {
		var cancel context.CancelFunc
		removeCtx, cancel = context.WithTimeout(ctx, options.timeout)
		defer cancel()
	}
	if err := r.manager.RemoveAgent(removeCtx, name); err != nil {
		// Drain failed (usually a deadline): keep the tombstone so new
		// sessions stay blocked, restore the agent so the registration
		// is still visible, and let the caller retry.
		if rerr := r.registry.Put(name, entry); rerr != nil {
			telemetry.WarnErr(ctx, "runtime: restore agent after remove drain failed", rerr,
				otellog.String(telemetry.AttrAgentID, name))
		}
		telemetry.Error(ctx, "runtime agent removal failed",
			otellog.String(telemetry.AttrAgentID, name),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		return err
	}
	if r.liveCatalog != nil {
		r.liveCatalog.Delete(name)
	}
	if err := entry.instance.Close(); err != nil {
		telemetry.Error(ctx, "runtime agent removal failed",
			otellog.String(telemetry.AttrAgentID, name),
			otellog.String(telemetry.AttrErrorMessage, err.Error()))
		return err
	}
	r.publishLifecycleEvent(ctx, SubjectAgentRemoved(name), AgentLifecycleEvent{
		AgentID:     name,
		Name:        entry.instance.Card.Name,
		Description: entry.instance.Card.Description,
	})
	telemetry.Info(ctx, "runtime agent removed",
		otellog.String(telemetry.AttrAgentID, name),
		otellog.String("agent.card.name", entry.instance.Card.Name))
	return nil
}

func validateAgentID(name string) error {
	if name == "" || strings.TrimSpace(name) != name {
		return errdefs.Validationf(
			"runtime: agent id must be non-empty and must not have surrounding whitespace")
	}
	return nil
}
