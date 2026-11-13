package runtime

import (
	"context"
	goruntime "runtime"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox-bwrap"
	"github.com/GizClaw/flowcraft/core/sandbox-local"
	"github.com/GizClaw/flowcraft/core/sandbox-remote"
	"github.com/GizClaw/flowcraft/core/sandbox-seatbelt"
	"github.com/GizClaw/flowcraft/core/sandbox-windows"
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

	var runner sandbox.Runner
	switch r.cfg.Sandbox.Backend {
	case BackendLocal:
		opts := make([]local.Option, 0, 2)
		if limits.MaxOutputBytes > 0 {
			opts = append(opts, local.WithMaxOutputBytes(limits.MaxOutputBytes))
		}
		if journalCfg != nil {
			opts = append(opts, local.WithFileJournalConfig(*journalCfg))
		}
		runner = local.New(dir, opts...)
	case BackendSeatbelt:
		opts := []seatbelt.RunnerOption{seatbelt.WithWritablePaths(writes...)}
		if c := r.cfg.Sandbox.Seatbelt; c != nil {
			if c.Binary != "" {
				opts = append(opts, seatbelt.WithBinary(c.Binary))
			}
			if c.ReadOnlyRoot {
				opts = append(opts, seatbelt.WithReadOnlyRoot())
			}
		}
		if journalCfg != nil {
			opts = append(opts, seatbelt.WithFileJournalConfig(*journalCfg))
		}
		runner, err = seatbelt.New(dir, opts...)
	case BackendBwrap:
		opts := []bwrap.RunnerOption{bwrap.WithWritablePaths(writes...)}
		if c := r.cfg.Sandbox.Bwrap; c != nil {
			if c.Binary != "" {
				opts = append(opts, bwrap.WithBinary(c.Binary))
			}
			if len(c.ExtraFlags) > 0 {
				opts = append(opts, bwrap.WithExtraFlags(c.ExtraFlags...))
			}
			if c.ReadOnlyRoot {
				opts = append(opts, bwrap.WithReadOnlyRoot())
			}
		}
		if journalCfg != nil {
			opts = append(opts, bwrap.WithFileJournalConfig(*journalCfg))
		}
		runner, err = bwrap.New(dir, opts...)
	case BackendWindows:
		opts := []windows.Option{windows.WithWritablePaths(writes...)}
		if c := r.cfg.Sandbox.Windows; c != nil && c.WriteConfine {
			opts = append(opts, windows.WithWriteConfinement())
		}
		if journalCfg != nil {
			opts = append(opts, windows.WithFileJournalConfig(*journalCfg))
		}
		runner, err = windows.New(dir, opts...)
	case BackendRemote:
		runner, err = r.cfg.Sandbox.remoteRunner(ctx)
	default:
		// Validate rejects this earlier; the switch keeps the boundary explicit.
		return errdefs.Validationf("sandbox.backend: unknown backend %q", r.cfg.Sandbox.Backend)
	}
	if err != nil {
		return errdefs.Validationf("sandbox.backend %q: %w", r.cfg.Sandbox.Backend, err)
	}
	// applySandboxPolicy returns nil on error, so close the runner it was given
	// rather than the decoration that does not exist.
	built, err := r.applySandboxPolicy(ctx, runner, limits)
	if err != nil {
		if cerr := runner.Close(); cerr != nil {
			telemetry.WarnErr(ctx, "sandbox: close the backend after a policy failure", cerr)
		}
		return err
	}
	runner = built
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

// applySandboxPolicy projects the document's sandbox policy onto every call.
//
// The document is the only place a deployment states its boundary, so the fields
// it sets have to reach the commands: without this, cpu_millicores, memory_mib and
// writes were parsed, validated and then ignored.
//
// Three rules, all "narrow, never widen":
//
//   - A cpu or memory budget becomes the Resources default on every call. A
//     backend that does not enforce the cap is a build error rather than a
//     dropped field, because running unmeasured is the failure mode these caps
//     exist to prevent.
//   - sandbox.writes becomes the per-call file boundary, but only where the
//     backend confines writes at the OS level. The local runner has no boundary
//     and rejects a per-call one, and a remote runner's boundary belongs to the
//     server's own document, so both warn instead of failing every command.
//   - sandbox.timeout is a ceiling: a tool may ask for a shorter deadline, never
//     a longer one (the decorator takes the smaller).
func (r *Runtime) applySandboxPolicy(ctx context.Context, runner sandbox.Runner, limits sandbox.ResourceLimits) (sandbox.Runner, error) {
	backend := r.cfg.Sandbox.Backend
	caps := runner.Capabilities().Policy

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

	timeout, err := r.cfg.Sandbox.timeout()
	if err != nil {
		return nil, err
	}
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
		case !caps.WriteConfinement:
			telemetry.Warn(ctx,
				"this backend cannot confine writes per call; sandbox.writes is only advisory here",
				attribute.String("sandbox.backend", backend))
		default:
			defaults.Permission = &permission.Policy{
				Writes: permission.Writes{Paths: writes},
			}
		}
	}
	return sandboxengine.WithDefaults(runner, defaults), nil
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
func (c SandboxConfig) remoteRunner(ctx context.Context) (sandbox.Runner, error) {
	opts, err := c.remoteOptions()
	if err != nil {
		return nil, err
	}
	transport, err := c.Remote.transport()
	if err != nil {
		return nil, err
	}
	if transport == remoteTransportUnix {
		if c.Remote.Token == "" && goruntime.GOOS == "windows" {
			// os.Chmod is advisory on Windows and the directory ACL is the real
			// boundary, so a socket there is not a credential by itself.
			telemetry.Warn(ctx,
				"sandbox.remote speaks over a unix socket without a token; on Windows "+
					"the socket's permissions are advisory, so keep a token or protect the directory",
				attribute.String(telemetry.AttrSandboxEndpoint, c.Remote.Socket))
		}
		runner, err := remote.NewUnix(ctx, c.Remote.Socket, opts...)
		if err != nil {
			return nil, errdefs.Validationf("sandbox.remote.socket: %w", err)
		}
		return runner, nil
	}
	return remote.New(ctx, c.Remote.Endpoint, opts...)
}

// remoteOptions translates the document's remote block into client options.
func (c SandboxConfig) remoteOptions() ([]remote.Option, error) {
	var opts []remote.Option
	if c.Remote.Token != "" {
		opts = append(opts, remote.WithToken(c.Remote.Token))
	}
	var httpOpts []remote.HTTPOption
	if c.Remote.InsecureSkipVerify {
		httpOpts = append(httpOpts, remote.WithHTTPInsecureTLS())
	}
	if timeout, err := c.Remote.timeout(); err != nil {
		return nil, err
	} else if timeout > 0 {
		httpOpts = append(httpOpts, remote.WithHTTPTimeout(timeout))
	}
	hold, err := c.Remote.pollHold()
	if err != nil {
		return nil, err
	}
	if len(httpOpts) > 0 {
		opts = append(opts, remote.WithHTTPOptions(httpOpts...))
	}
	if hold > 0 {
		opts = append(opts, remote.WithPollHold(hold))
	}
	return opts, nil
}
