package runtime

import (
	"context"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/bwrap"
	"github.com/GizClaw/flowcraft/core/sandbox/local"
	"github.com/GizClaw/flowcraft/core/sandbox/seatbelt"
	"github.com/GizClaw/flowcraft/core/sandbox/windows"
	"github.com/GizClaw/flowcraft/core/skill"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool-builtin/exec"
	"github.com/GizClaw/flowcraft/core/tool-builtin/files"
	"github.com/GizClaw/flowcraft/core/tool-builtin/skills"
	toolmcp "github.com/GizClaw/flowcraft/core/tool-mcp"
	"github.com/GizClaw/flowcraft/core/tool/engine"
)

// build creates every resource in construction order and records it for
// [Runtime.Close]. An error leaves the runtime half built, so the caller's
// cleanup path (New) closes whatever exists.
func (r *Runtime) build(ctx context.Context, drivers map[string]DriverFunc, selectors map[string]Selector) error {
	if err := r.buildTree(); err != nil {
		return err
	}
	if err := r.buildRunner(ctx); err != nil {
		return err
	}
	if err := r.buildTools(ctx); err != nil {
		return err
	}
	return r.buildProvider(drivers, selectors)
}

func (r *Runtime) buildTree() error {
	tree, err := fsx.Open(fsx.WithWritable(r.cfg.Files.Roots...))
	if err != nil {
		return errdefs.Validationf("files.roots: %w", err)
	}
	r.tree = tree
	r.closers = append(r.closers, tree)
	return nil
}

func (r *Runtime) buildRunner(ctx context.Context) error {
	limits, err := r.cfg.Sandbox.limits()
	if err != nil {
		return err
	}
	dir := r.cfg.workDir()
	writes := r.cfg.Sandbox.Writes

	var runner sandbox.Runner
	switch r.cfg.Sandbox.Backend {
	case BackendLocal:
		opts := make([]local.Option, 0, 1)
		if limits.MaxOutputBytes > 0 {
			opts = append(opts, local.WithMaxOutputBytes(limits.MaxOutputBytes))
		}
		runner = local.New(dir, opts...)
	case BackendSeatbelt:
		opts := []seatbelt.RunnerOption{seatbelt.WithWritablePaths(writes...)}
		runner, err = seatbelt.New(dir, opts...)
	case BackendBwrap:
		opts := []bwrap.RunnerOption{bwrap.WithWritablePaths(writes...)}
		runner, err = bwrap.New(dir, opts...)
	case BackendWindows:
		runner, err = windows.New(dir)
	default:
		// Validate rejects this earlier; the switch keeps the boundary explicit.
		return errdefs.Validationf("sandbox.backend: unknown backend %q", r.cfg.Sandbox.Backend)
	}
	if err != nil {
		return errdefs.Validationf("sandbox.backend %q: %w", r.cfg.Sandbox.Backend, err)
	}
	r.runner = runner
	r.closers = append(r.closers, runner)

	if !r.cfg.Sandbox.Journal {
		return nil
	}
	journal, err := sandbox.OpenJournal(ctx, runner)
	if err != nil {
		return errdefs.Validationf(
			"sandbox.journal: %q does not serve a file journal: %w", r.cfg.Sandbox.Backend, err)
	}
	r.journal = journal
	r.closers = append(r.closers, journal)
	return nil
}

func (r *Runtime) buildTools(ctx context.Context) error {
	sources, err := r.buildSources(ctx)
	if err != nil {
		return err
	}
	table, err := middlewareTable(r.middlewareOptions)
	if err != nil {
		return err
	}
	chain, err := r.middlewareChain(ctx, table)
	if err != nil {
		return err
	}
	options := []engine.AssemblyOption{engine.WithMiddleware(chain...)}
	if dynamic := r.cfg.Tools.Dynamic; dynamic != nil {
		policy, err := dynamic.policy()
		if err != nil {
			return err
		}
		// WithDynamic also registers tool_search as an always-visible tool, so a
		// deferred tool set is never unreachable.
		options = append(options, engine.WithDynamic(policy))
	}
	assembly, err := engine.NewAssembly(sources, options...)
	if err != nil {
		return errdefs.Validationf("tools: %w", err)
	}
	r.assembly = assembly
	r.registry = assembly.Registry()
	r.closers = append(r.closers, assembly)
	return nil
}

// buildSources wires each enabled tool set. Each set contributes a tool.Source,
// so the assembly decides visibility and the sources only describe tools.
func (r *Runtime) buildSources(ctx context.Context) ([]tool.Source, error) {
	sources := make([]tool.Source, 0, 4)

	if r.cfg.Tools.enabled(ToolFiles) {
		group, err := files.New(r.tree, r.cfg.workDir(), files.WithImages(r.cfg.Tools.Images))
		if err != nil {
			return nil, errdefs.Validationf("tools.files: %w", err)
		}
		sources = append(sources, group)
	}

	if r.cfg.Tools.enabled(ToolExec) {
		policy, err := r.cfg.Sandbox.permission()
		if err != nil {
			return nil, err
		}
		command, err := exec.NewCommand(r.runner,
			exec.WithWorkDir(r.cfg.workDir()),
			exec.WithNet(policy.Net))
		if err != nil {
			return nil, errdefs.Validationf("tools.exec: %w", err)
		}
		sessionGroup, err := exec.NewSession(r.runner,
			exec.WithTTY(r.cfg.Tools.TTY),
			exec.WithSessionNet(policy.Net),
			exec.WithWorkdirHint(r.cfg.workDir()))
		if err != nil {
			return nil, errdefs.Validationf("tools.exec: %w", err)
		}
		r.sessions = sessionGroup
		// The session group is closed before the assembly, so a tool never sees
		// a group that is already shut.
		r.closers = append(r.closers, sessionGroup)
		sources = append(sources, singleSource{command}, sessionGroup)
	}

	if r.cfg.Tools.enabled(ToolSkills) && len(r.cfg.Skills.Roots) > 0 {
		outcome := skill.Discover(ctx, r.tree, r.cfg.Skills.Roots)
		index, err := skill.NewIndex(outcome.Skills)
		if err != nil {
			return nil, errdefs.Validationf("skills: %w", err)
		}
		group, err := skills.New(r.tree, outcome.Skills)
		if err != nil {
			return nil, errdefs.Validationf("tools.skills: %w", err)
		}
		r.skills = index
		sources = append(sources, group)
	}

	if len(r.cfg.Tools.MCP.Servers) > 0 {
		spec, err := r.cfg.Tools.MCP.spec()
		if err != nil {
			return nil, err
		}
		// Two modes, one visible difference: eager starts every server at
		// assembly, deferred starts one when a tool from it is loaded.
		var source *toolmcp.Source
		if r.cfg.Tools.MCP.deferred() {
			source, err = toolmcp.NewLazy(spec)
		} else {
			source, err = toolmcp.Connect(ctx, spec)
		}
		if err != nil {
			return nil, errdefs.Validationf("mcp: %w", err)
		}
		// Servers hold processes and connections; they close before the assembly
		// so no registry still hands out a tool whose server is gone.
		r.closers = append(r.closers, source)
		sources = append(sources, source)
	}

	return sources, nil
}

// singleSource publishes one tool as a Source, for tools that are not grouped.
type singleSource struct{ tool tool.Tool }

func (s singleSource) Tools() []tool.Tool         { return []tool.Tool{s.tool} }
func (s singleSource) LazyTools() []tool.LazyTool { return nil }

func (r *Runtime) buildProvider(drivers map[string]DriverFunc, selectors map[string]Selector) error {
	entries := r.cfg.Inference.Models
	if len(entries) == 0 {
		// A tools-only runtime is legitimate: not every host needs a model.
		return nil
	}

	providers := make(map[string]ProviderConfig, len(r.cfg.Inference.Providers))
	for _, provider := range r.cfg.Inference.Providers {
		providers[provider.Name] = provider
	}

	// One provider is constructed per block, so ten models declared on one block
	// share a client, a connection pool and a credential instead of rebuilding
	// them ten times.
	built := make(map[string]inference.Provider, len(providers))
	targets := make([]route.Target, 0, len(entries))
	for _, entry := range entries {
		block := providers[entry.Provider]
		provider, constructed := built[block.Name]
		if !constructed {
			build, ok := drivers[block.Driver]
			if !ok {
				return errdefs.Validationf(
					"inference.providers[%s].driver: %q is not available; offered: %s",
					block.Name, block.Driver, offeredDrivers(drivers))
			}
			// Assign to the outer provider: shadowing it here left every target
			// with a zero value, which route reports as "serves no seam".
			constructedProvider, err := build(block.Settings)
			if err != nil {
				return errdefs.Validationf(
					"inference.providers[%s].driver %q: %w", block.Name, block.Driver, err)
			}
			if err := constructedProvider.Validate(); err != nil {
				return errdefs.Validationf(
					"inference.providers[%s].driver %q: %w", block.Name, block.Driver, err)
			}
			provider = constructedProvider
			built[block.Name] = provider
		}

		descriptor, err := entry.descriptor(block)
		if err != nil {
			return errdefs.Validationf("inference.models[%s]: %w", entry.targetName(), err)
		}
		catalog, err := mergeCatalog(block.Name, provider.Catalog, []model.ModelDescriptor{descriptor})
		if err != nil {
			return err
		}
		provider.Catalog = catalog
		// Keep the merged catalog on the block so the next model extends it
		// rather than replacing it.
		built[block.Name] = provider

		targets = append(targets, route.Target{
			Name:     entry.targetName(),
			Provider: provider,
			Weight:   entry.Weight,
			Model:    descriptor.ID,
			Profile:  block.Name,
		})
	}

	policy := route.Policy{}
	retry, err := r.cfg.Inference.Route.retry()
	if err != nil {
		return err
	}
	policy.Retry = retry
	if name := strings.TrimSpace(r.cfg.Inference.Route.Selector); name != "" {
		selector, ok := selectors[name]
		if !ok {
			return errdefs.Validationf(
				"inference.route.selector: %q is not available; offered: %s",
				name, offeredSelectors(selectors))
		}
		settings := r.cfg.Inference.Route.SelectorSettings
		if selector.Generate != nil {
			built, err := selector.Generate(settings)
			if err != nil {
				return errdefs.Validationf("inference.route.selector %q: %w", name, err)
			}
			policy.Generate = built
		}
		if selector.Decision != nil {
			built, err := selector.Decision(settings)
			if err != nil {
				return errdefs.Validationf("inference.route.selector %q: %w", name, err)
			}
			policy.Decision = built
		}
	}

	router, err := route.New(targets, policy)
	if err != nil {
		return errdefs.Validationf("inference: %w", err)
	}
	r.router = router
	r.provider = router.Provider()
	return nil
}

func offeredSelectors(selectors map[string]Selector) string {
	if len(selectors) == 0 {
		return "none (pass runtime.WithSelectors)"
	}
	names := make([]string, 0, len(selectors))
	for name := range selectors {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func offeredDrivers(drivers map[string]DriverFunc) string {
	if len(drivers) == 0 {
		return "none (pass runtime.WithDrivers)"
	}
	names := make([]string, 0, len(drivers))
	for name := range drivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
