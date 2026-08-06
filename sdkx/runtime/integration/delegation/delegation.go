// Package delegation provides the local delegation Runtime integration.
package delegation

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkdelegation "github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdkx/delegation"
	runtimecore "github.com/GizClaw/flowcraft/sdkx/runtime"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
	tooldelegation "github.com/GizClaw/flowcraft/sdkx/tool/delegation"
)

const (
	// Kind is the runtime integration catalog kind.
	Kind = "delegation.local"

	backendDependencyKind = "delegation.AsyncBackend"
)

// Factory prepares local delegation integrations over a shared tool registry.
type Factory struct {
	tools *tool.Registry
}

// NewFactory constructs a local delegation integration factory.
func NewFactory(tools *tool.Registry) (*Factory, error) {
	if tools == nil {
		return nil, errdefs.Validationf("delegation runtime integration: tool registry is required")
	}
	return &Factory{tools: tools}, nil
}

// Spec declares the optional asynchronous backend dependency.
func (*Factory) Spec() runtimecore.IntegrationSpec {
	return runtimecore.IntegrationSpec{
		Kind: Kind,
		Deps: []runtimecore.DependencySpec{{
			Name: "backend", Kind: backendDependencyKind,
			Type: reflect.TypeFor[delegation.AsyncBackend](), Required: false,
		}},
	}
}

type settings struct {
	MaxConcurrency *int    `json:"max_concurrency,omitempty"`
	MaxDepth       *int    `json:"max_depth,omitempty"`
	Timeout        *string `json:"timeout,omitempty"`
}

// Prepare strictly decodes settings and registers delegation tools early
// enough for a subsequent tool.Assembly deployment build to observe them.
func (f *Factory) Prepare(_ context.Context, input runtimecore.PrepareInput) (runtimecore.PreparedIntegration, error) {
	if f == nil || f.tools == nil {
		return nil, errdefs.Validationf("delegation runtime integration: tool registry is required")
	}
	var wire settings
	if err := input.Settings.Decode(&wire); err != nil {
		return nil, errdefs.Validation(fmt.Errorf(
			"delegation runtime integration %q: decode settings: %w", input.Name, err))
	}
	options := []delegation.Option{delegation.WithDeferredWorkers()}
	if wire.MaxConcurrency != nil {
		if *wire.MaxConcurrency <= 0 {
			return nil, errdefs.Validationf(
				"delegation runtime integration %q: max_concurrency must be positive",
				input.Name)
		}
		options = append(options, delegation.WithMaxConcurrency(*wire.MaxConcurrency))
	}
	if wire.MaxDepth != nil {
		if *wire.MaxDepth <= 0 {
			return nil, errdefs.Validationf(
				"delegation runtime integration %q: max_depth must be positive",
				input.Name)
		}
		options = append(options, delegation.WithMaxDepth(*wire.MaxDepth))
	}
	if wire.Timeout != nil {
		timeout, parseErr := time.ParseDuration(*wire.Timeout)
		if parseErr != nil {
			return nil, errdefs.Validation(fmt.Errorf(
				"delegation runtime integration %q: timeout %q: %w",
				input.Name, *wire.Timeout, parseErr))
		}
		if timeout < 0 {
			return nil, errdefs.Validationf(
				"delegation runtime integration %q: timeout cannot be negative",
				input.Name)
		}
		options = append(options, delegation.WithTimeout(timeout))
	}

	directory := delegation.NewDirectory()
	integrationTools := tooldelegation.New(directory)
	releaseTools, ok := f.tools.RegisterAllIfAbsent(integrationTools...)
	if !ok {
		return nil, errdefs.Conflictf(
			"delegation runtime integration %q: a delegation tool is already registered",
			input.Name)
	}
	return &integration{
		name: input.Name, releaseTools: releaseTools,
		directory: directory, serviceOptions: options,
	}, nil
}

type integration struct {
	name           string
	releaseTools   func()
	directory      *delegation.Directory
	serviceOptions []delegation.Option

	mu      sync.Mutex
	bound   bool
	service *delegation.Service

	closeOnce sync.Once
	closeErr  error
}

// Bind binds the read-only deployment view and constructs a deferred Service.
func (i *integration) Bind(ctx context.Context, input runtimecore.BindInput) error {
	if i == nil {
		return errdefs.Validationf("delegation runtime integration: nil integration")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.bound {
		return errdefs.Conflictf("delegation runtime integration %q: already bound", i.name)
	}
	if input.Deployment == nil {
		return errdefs.Validationf("delegation runtime integration %q: deployment is required", i.name)
	}
	if input.BaseHosts == nil {
		return errdefs.Validationf("delegation runtime integration %q: base hosts are required", i.name)
	}
	workerHost, err := input.BaseHosts.NewHost(ctx, session.HostRequest{
		Key: session.Key{
			AgentID:   "delegation-worker",
			ContextID: "delegation-worker-" + i.name,
		},
		RunID:      "delegation-worker-" + i.name,
		Interrupts: make(chan agent.Interrupt),
		AskUser: func(context.Context, agent.UserPrompt) (agent.UserReply, error) {
			return agent.UserReply{}, errdefs.NotAvailablef(
				"delegation runtime integration: worker AskUser is unavailable")
		},
	})
	if err != nil {
		return fmt.Errorf("delegation runtime integration %q: create worker host: %w", i.name, err)
	}
	if err := i.directory.Bind(input.Deployment); err != nil {
		return err
	}
	var backend delegation.AsyncBackend
	if _, configured := input.Dependencies.Get("backend"); configured {
		backend, err = runtimecore.DependencyAs[delegation.AsyncBackend](input.Dependencies, "backend")
		if err != nil {
			return err
		}
	}
	options := append([]delegation.Option(nil), i.serviceOptions...)
	options = append(options, delegation.WithWorkerHost(workerHost))
	service, err := delegation.NewService(i.directory, backend, options...)
	if err != nil {
		return err
	}
	i.service = service
	i.bound = true
	return nil
}

// DecorateHost exposes the bound delegation service on every turn Host.
func (i *integration) DecorateHost(base session.HostFactory) (session.HostFactory, error) {
	if i == nil || base == nil {
		return nil, errdefs.Validationf("delegation runtime integration: base HostFactory is required")
	}
	i.mu.Lock()
	service := i.service
	i.mu.Unlock()
	if service == nil {
		return nil, errdefs.NotAvailablef(
			"delegation runtime integration %q: service is not bound", i.name)
	}
	return session.HostFactoryFunc(func(ctx context.Context, request session.HostRequest) (agent.Host, error) {
		host, err := base.NewHost(ctx, request)
		if err != nil {
			return nil, err
		}
		return sdkdelegation.WithService(host, service), nil
	}), nil
}

// Start begins deferred asynchronous workers.
func (i *integration) Start(context.Context) error {
	if i == nil {
		return errdefs.NotAvailablef("delegation runtime integration: nil integration")
	}
	i.mu.Lock()
	service := i.service
	i.mu.Unlock()
	if service == nil {
		return errdefs.NotAvailablef(
			"delegation runtime integration %q: service is not bound", i.name)
	}
	return service.Start()
}

// Close stops the Service before unregistering only tools still owned by this
// integration. It is safe before Bind and on repeated calls.
func (i *integration) Close() error {
	if i == nil {
		return nil
	}
	i.closeOnce.Do(func() {
		i.mu.Lock()
		service := i.service
		i.mu.Unlock()
		if service != nil {
			i.closeErr = service.Close()
		}
		if i.releaseTools != nil {
			i.releaseTools()
		}
	})
	return i.closeErr
}

var (
	_ runtimecore.IntegrationFactory  = (*Factory)(nil)
	_ runtimecore.PreparedIntegration = (*integration)(nil)
)
