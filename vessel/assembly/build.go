package assembly

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/memory/recall/ops"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/vessel"
)

type Option func(*buildConfig)

type buildConfig struct {
	catalog       *Catalog
	defaults      Defaults
	vesselOptions []vessel.Option
}

func WithCatalog(catalog *Catalog) Option {
	return func(cfg *buildConfig) { cfg.catalog = catalog }
}

// WithDefaults controls how omitted manifest backend fields are interpreted.
func WithDefaults(defaults Defaults) Option {
	return func(cfg *buildConfig) { cfg.defaults = defaults }
}

func WithVesselOptions(opts ...vessel.Option) Option {
	return func(cfg *buildConfig) {
		cfg.vesselOptions = append(cfg.vesselOptions, opts...)
	}
}

// Build constructs all declared resources and returns a runnable Captain.
func Build(ctx context.Context, m Manifest, opts ...Option) (*Assembly, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg := buildConfig{catalog: NewCatalog(), defaults: DefaultDefaults()}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	cfg.defaults = normalizeDefaults(cfg.defaults)
	if cfg.catalog == nil {
		cfg.catalog = NewCatalog()
	}
	if err := m.ValidateWithCatalog(cfg.defaults, cfg.catalog); err != nil {
		return nil, err
	}

	var closers []func(context.Context) error
	closeOnError := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			_ = closers[i](context.Background())
		}
	}

	wb, err := buildWorkspace(ctx, m.Workspace, cfg.defaults, cfg.catalog)
	if err != nil {
		return nil, err
	}
	closers = append(closers, wb.closers...)

	rb, err := buildRecall(ctx, m.Recall, wb.workspace, cfg.defaults, cfg.catalog, m)
	if err != nil {
		closeOnError()
		return nil, err
	}
	closers = append(closers, rb.closers...)

	kb, err := buildKnowledge(ctx, m.Knowledge, wb.workspace, cfg.defaults, cfg.catalog, m)
	if err != nil {
		closeOnError()
		return nil, err
	}
	closers = append(closers, kb.closers...)

	registry, err := buildToolRegistry(ctx, m, cfg.catalog, wb.workspace, rb.memory, kb.service)
	if err != nil {
		closeOnError()
		return nil, err
	}

	runner, target, err := buildRecallOps(m, m.Recall, rb)
	if err != nil {
		closeOnError()
		return nil, err
	}

	vopts := []vessel.Option{
		vessel.WithEngineFactory(dispatchEngineFactory(cfg.catalog)),
		vessel.WithToolRegistry(registry),
		vessel.WithSessionStore(wb.session),
	}
	if resolver := buildLLMResolver(m, cfg.catalog); resolver != nil {
		vopts = append(vopts, vessel.WithLLMResolver(resolver))
	}
	vopts = append(vopts, cfg.vesselOptions...)
	captain, err := vessel.New(m.VesselSpecWithDefaults(cfg.defaults), vopts...)
	if err != nil {
		closeOnError()
		return nil, err
	}

	return &Assembly{
		Manifest:  m,
		Captain:   captain,
		Workspace: wb.workspace,
		Recall:    rb.memory,
		Knowledge: kb.service,
		Tools:     registry,
		OpsRunner: runner,
		opsTarget: target,
		closers:   closers,
	}, nil
}

func buildLLMResolver(m Manifest, catalog *Catalog) llm.LLMResolver {
	resolver := catalog.llmResolver()
	if resolver == nil || m.LLM == nil || m.LLM.Default == "" {
		return resolver
	}
	return defaultModelResolver{defaultModel: m.LLM.Default, inner: resolver}
}

type defaultModelResolver struct {
	defaultModel string
	inner        llm.LLMResolver
}

func (r defaultModelResolver) Resolve(ctx context.Context, model string) (llm.LLM, error) {
	if model == "" {
		model = r.defaultModel
	}
	return r.inner.Resolve(ctx, model)
}

func (r defaultModelResolver) InvalidateCache(opts ...llm.InvalidateOption) {
	r.inner.InvalidateCache(opts...)
}

// StartOps explicitly starts recall worker loops when recall.ops.enabled=true.
func (a *Assembly) StartOps(ctx context.Context) error {
	if a == nil {
		return errdefs.Validationf("vessel assembly: nil assembly")
	}
	if a.OpsRunner == nil {
		return nil
	}
	if a.OpsSupervisor != nil {
		return errdefs.Conflictf("vessel assembly: recall ops already started")
	}
	supervisor, err := ops.Start(ctx, a.OpsRunner, a.opsTarget)
	if err != nil {
		return err
	}
	a.OpsSupervisor = supervisor
	return nil
}

func (a *Assembly) StopOps() error {
	if a == nil || a.OpsSupervisor == nil {
		return nil
	}
	err := a.OpsSupervisor.Stop()
	a.OpsSupervisor = nil
	return err
}

// Close stops the Captain and ops loops, then releases memory/knowledge
// backends owned by this Assembly.
func (a *Assembly) Close() error {
	if a == nil {
		return nil
	}
	var errs []error
	if err := a.StopOps(); err != nil {
		errs = append(errs, err)
	}
	if a.Captain != nil {
		if err := a.Captain.Stop(context.Background()); err != nil {
			errs = append(errs, err)
		}
	}
	for i := len(a.closers) - 1; i >= 0; i-- {
		if err := a.closers[i](context.Background()); err != nil {
			errs = append(errs, err)
		}
	}
	a.closers = nil
	return errors.Join(errs...)
}
