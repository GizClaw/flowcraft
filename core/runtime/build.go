package runtime

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	sandboxengine "github.com/GizClaw/flowcraft/core/sandbox/engine"
	"github.com/GizClaw/flowcraft/core/sandbox/journal"
	"github.com/GizClaw/flowcraft/core/skill"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool-builtin/exec"
	"github.com/GizClaw/flowcraft/core/tool-builtin/files"
	"github.com/GizClaw/flowcraft/core/tool-builtin/skills"
	toolmcp "github.com/GizClaw/flowcraft/core/tool-mcp"
	"github.com/GizClaw/flowcraft/core/tool/engine"

	"go.opentelemetry.io/otel/attribute"
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

	// The journal block is opt-in by presence; resolving it here means a bad
	// exclude, retention or op name fails the build instead of producing a runner
	// that reports nothing.
	journalCfg, err := r.cfg.Sandbox.journalConfig(dir, writes)
	if err != nil {
		return err
	}

	build, ok := r.sandboxBackends[r.cfg.Sandbox.Backend]
	if !ok {
		return errdefs.Validationf("sandbox.backend: unknown backend %q (registered: %s)",
			r.cfg.Sandbox.Backend, offeredBackends(r.sandboxBackends))
	}
	runner, err := build(ctx, SandboxBuild{
		Dir:     dir,
		Writes:  writes,
		Limits:  limits,
		Journal: journalCfg,
		Config:  r.cfg.Sandbox,
	})
	if err != nil {
		return errdefs.Validationf("sandbox.backend %q: %w", r.cfg.Sandbox.Backend, err)
	}
	// resolveSandbox runs after the backend exists because the write projection
	// depends on what the backend declares it can confine.
	spec, err := r.resolveSandbox(ctx, limits, runner.Capabilities())
	if err != nil {
		r.closeBackend(ctx, runner)
		return err
	}
	// applySandboxPolicy returns nil on error, so close the runner it was given
	// rather than the decoration that does not exist.
	built, err := r.applySandboxPolicy(runner, spec)
	if err != nil {
		r.closeBackend(ctx, runner)
		return err
	}
	runner = built
	if r.sandboxDecorator != nil {
		decorated, err := r.sandboxDecorator(spec, runner)
		if err != nil {
			r.closeBackend(ctx, runner)
			// Name the seam and keep the host's error kind: an operator seeing a
			// bare host error cannot tell which build step produced it.
			return fmt.Errorf("sandbox: WithSandboxDecorator: %w", err)
		}
		if decorated == nil {
			r.closeBackend(ctx, runner)
			return errdefs.Validationf("sandbox: WithSandboxDecorator returned a nil runner")
		}
		runner = decorated
	}
	r.runner = runner
	r.closers = append(r.closers, runner)

	// The file tools work on this host's filesystem; a remote backend runs the
	// commands on another one. Say it out loud: silently having the shell write
	// files the tools cannot see is the shape of failure nobody debugs quickly.
	if r.cfg.Sandbox.Backend == BackendRemote && r.cfg.Tools.enabled(ToolFiles) {
		telemetry.Warn(ctx,
			"sandbox.backend is remote while tools.files work on this host: "+
				"mount the same workspace on both sides, or disable tools.files",
			attribute.String(telemetry.AttrSandboxEndpoint, r.cfg.Sandbox.Remote.Endpoint))
	}

	if journalCfg == nil {
		return nil
	}
	journal, err := sandbox.OpenJournal(ctx, runner)
	if err != nil {
		// A decorator that drops JournalProvider is the common cause here, and
		// blaming the backend for it sends the operator to the wrong file: the
		// composed runner still advertised the journal, the wrapper swallowed it.
		if r.sandboxDecorator != nil {
			if _, ok := built.(sandbox.JournalProvider); ok {
				return errdefs.Validationf(
					"sandbox.journal: the decorated runner (%T) does not forward OpenJournal; "+
						"wrap with engine.Forwarder: %w", runner, err)
			}
		}
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
	if decorator := r.cfg.Tools.Descriptions.withDefinitionDecorator(ctx); decorator != nil {
		options = append(options, engine.WithDefinitionDecorator(decorator))
	}
	assembly, err := engine.NewAssembly(sources, options...)
	if err != nil {
		return errdefs.Validationf("tools: %w", err)
	}
	r.assembly = assembly
	r.registry = assembly.Registry()
	r.closers = append(r.closers, assembly)
	// A description names a tool, and a tool only exists once its source has
	// published it — after the assembly, because that is when a deferred source
	// attaches. Registered before the check, so the caller's cleanup path closes a
	// rejected assembly like any other half-built resource.
	if err := r.cfg.Tools.Descriptions.validateAgainst(r.registry); err != nil {
		return err
	}
	return nil
}

// buildSources wires each enabled tool set. Each set contributes a tool.Source,
// so the assembly decides visibility and the sources only describe tools.
func (r *Runtime) buildSources(ctx context.Context) ([]tool.Source, error) {
	sources := make([]tool.Source, 0, 4+len(r.sources))
	sources = append(sources, r.sources...)

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
		group, err := skills.New(r.tree, index)
		if err != nil {
			return nil, errdefs.Validationf("tools.skills: %w", err)
		}
		r.skills = index
		// Parse failures and name/directory mismatches do not fail discovery, so
		// they have to be said out loud here or they never reach anyone: the
		// catalog silently gets smaller and the author never learns why.
		for i, diagnostic := range outcome.Diagnostics {
			if i >= maxSkillDiagnosticsLogged {
				telemetry.Warn(ctx, "skills: further problems were not logged",
					attribute.Int("skills.diagnostics.total", len(outcome.Diagnostics)))
				break
			}
			telemetry.Warn(ctx, "skills: a skill file was skipped",
				attribute.String("skills.path", diagnostic.Path),
				attribute.String(telemetry.AttrErrorMessage, diagnostic.Message))
		}
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

// resolveSandbox projects the document's sandbox section onto the values every
// call is merged with, and onto the [Sandbox] a decorator receives.
//
// The document is the only place a deployment states its boundary, so the fields
// it sets have to reach the commands: without this, cpu_millicores, memory_mib and
// writes were parsed, validated and then ignored.
//
// Four rules, all "narrow, never widen":
//
//   - A cpu or memory budget becomes the Resources default on every call. A
//     backend that does not enforce the cap is a build error rather than a
//     dropped field, because running unmeasured is the failure mode these caps
//     exist to prevent.
//   - sandbox.writes becomes the per-call file boundary, but only where the
//     backend confines writes at the OS level. The local runner has no boundary
//     and rejects a per-call one, and a remote runner's boundary belongs to the
//     server's own document, so both warn instead of failing every command.
//   - sandbox.net becomes the per-call network posture, under the same rule:
//     only a backend that declares the mode in Capabilities.NetModes gets it,
//     and a remote runner reports the server's declaration, so one test answers
//     for both. Not projecting it would discard the policy silently, because
//     defaults win entirely: the empty default replaces whatever the tool
//     passed. A tool's own Net never wins either direction — the deployment's
//     posture is the only one this projection carries.
//   - sandbox.timeout is a ceiling: a tool may ask for a shorter deadline, never
//     a longer one (the decorator takes the smaller).
//
// It runs after the backend exists because two of those decisions depend on what
// the backend declares it can enforce.
func (r *Runtime) resolveSandbox(ctx context.Context, limits sandbox.ResourceLimits, caps sandbox.Capabilities) (Sandbox, error) {
	policy, err := r.cfg.Sandbox.permission()
	if err != nil {
		return Sandbox{}, err
	}
	timeout, err := r.cfg.Sandbox.timeout()
	if err != nil {
		return Sandbox{}, err
	}
	backend := r.cfg.Sandbox.Backend
	defaults := sandbox.ExecOptions{
		Timeout: timeout,
		Resources: sandbox.ResourceLimits{
			CPUMillicores: limits.CPUMillicores,
			MemoryBytes:   limits.MemoryBytes,
		},
	}

	writes := r.cfg.Sandbox.Writes
	if len(writes) > 0 {
		switch {
		case backend == BackendRemote:
			telemetry.Warn(ctx,
				"sandbox.writes is decided by the server's own document; this list does not reach it",
				attribute.String(telemetry.AttrSandboxEndpoint, r.cfg.Sandbox.Remote.Endpoint))
		case !caps.Policy.WriteConfinement:
			telemetry.Warn(ctx,
				"this backend cannot confine writes per call; sandbox.writes is only advisory here",
				attribute.String("sandbox.backend", backend))
		default:
			defaults.Permission = &permission.Policy{
				Writes: permission.Writes{Paths: writes},
			}
		}
	}
	// sandbox.net is projected when the backend declares it can enforce the mode.
	// A backend that cannot keeps the host posture and says so: projecting would
	// fail every command on a boundary it does not have, and dropping it silently
	// is what this projection exists to stop.
	if mode := policy.Net.Mode; mode != permission.NetDefault {
		if slices.Contains(caps.Policy.NetModes, mode) {
			defaults.Net = policy.Net
		} else {
			telemetry.Warn(ctx,
				"this backend cannot enforce sandbox.net at the OS level; it is advisory here",
				attribute.String("sandbox.backend", backend))
		}
	}
	return Sandbox{
		Config:   r.cfg,
		WorkDir:  r.cfg.workDir(),
		Net:      policy.Net,
		Defaults: defaults,
	}, nil
}

// applySandboxPolicy fixes the resolved defaults onto every call, after checking
// that the backend can enforce the budgets they carry.
func (r *Runtime) applySandboxPolicy(runner sandbox.Runner, spec Sandbox) (sandbox.Runner, error) {
	backend := r.cfg.Sandbox.Backend
	caps := runner.Capabilities().Policy
	limits := spec.Defaults.Resources

	if limits.CPUMillicores > 0 && !caps.CPUCap {
		return nil, errdefs.Validationf(
			"sandbox.cpu_millicores: backend %q cannot enforce a cpu budget; "+
				"drop it or pick a backend that samples the process group", backend)
	}
	if limits.MemoryBytes > 0 && !caps.MemoryCap {
		return nil, errdefs.Validationf(
			"sandbox.memory_mib: backend %q cannot enforce a memory budget; "+
				"drop it or pick a backend that samples the process group", backend)
	}
	return sandboxengine.WithDefaults(runner, spec.Defaults), nil
}

// closeBackend releases a backend that failed a build step and is not
// registered as a closer yet: nothing else owns it at that point.
func (r *Runtime) closeBackend(ctx context.Context, runner sandbox.Runner) {
	if err := runner.Close(); err != nil {
		telemetry.WarnErr(ctx, "sandbox: close the backend after a build step failed", err)
	}
}

// journalConfig resolves the journal block into engine configuration, or nil when
// the deployment did not ask for one. The block's presence is the opt-in, so its
// zero value is a fully enabled journal with every implementation default.
func (c SandboxConfig) journalConfig(root string, extraRoots []string) (*journal.Config, error) {
	if c.Journal == nil {
		return nil, nil
	}
	cfg, err := c.Journal.Build(root, extraRoots)
	if err != nil {
		return nil, errdefs.Validationf("sandbox.journal: %w", err)
	}
	return &cfg, nil
}

// maxSkillDiagnosticsLogged bounds how many skipped skill files one runtime logs:
// a broken tree should be visible, not a wall of warnings.
const maxSkillDiagnosticsLogged = 5

// remoteRunner connects to the remote boundary described by the document.
//
// The transport is the document's choice: an HTTP endpoint across hosts, or a unix
// socket on this one. The client is identical either way — only where the bytes
// travel differs — so this builds the options once and picks the constructor.
