// Package resource adapts the graph engine to deployment resources:
// it registers the agent.Engine/graph factory that compiles a
// [graph.GraphDefinition] with node types wired from resource deps.
package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/errdefs"
	coregraph "github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/route"
	res "github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/utils/decode"
	"github.com/GizClaw/flowcraft/core/workspace"

	"go.opentelemetry.io/otel/attribute"
)

// Stable dependency names used by deployment documents. The capability names
// ("inference" … "script_runtime") are declared by the node type and script
// surface resources that consume them; only the two engine deps are declared by
// agent.Engine/graph itself.
const (
	DepInference      = "inference"
	DepRouter         = "router"
	DepTools          = "tools"
	DepWorkspace      = "workspace"
	DepSandbox        = "sandbox"
	DepScriptRuntime  = "script_runtime"
	DepNodeType       = "node_type"
	DepScriptBindings = "script_bindings"
)

// Typed dependency handles: one declaration each, shared by every factory in
// this package that accepts the dependency. The deployment plan checks the
// producer's Go type against these, so a document cannot wire a tool registry
// where an assembly is required.
var (
	depInference = res.Optional[*inference.Assembly](DepInference)
	depRouter    = res.Optional[*route.Router](DepRouter)
	depTools     = res.Optional[*tool.Assembly](DepTools)
	depWorkspace = res.Optional[workspace.Workspace](DepWorkspace)
	depSandbox   = res.Optional[sandbox.Runner](DepSandbox)
	depNodeTypes = res.OptionalMany[coregraph.NodeTypeRegistrar](DepNodeType)
	depBindings  = res.Optional[bindings.Provider](DepScriptBindings)

	// depToolsRequired is depTools as a mandatory dependency: the "tool"
	// node type cannot exist without a dispatcher, while the other
	// consumers of the same capability treat it as optional.
	depToolsRequired = res.Require[*tool.Assembly](DepTools)

	// depScriptRuntimes is a Many dep because a script node type selects a
	// runtime by name in its node config: every runtime is mounted as
	// "script_runtime.<name>", so one type serves a JS node and a Lua node
	// of the same graph.
	depScriptRuntimes = res.RequireMany[agent.ScriptRuntime](DepScriptRuntime)
)

// Settings is the strict settings subtree of the graph engine factory: the
// graph definition source plus build knobs.
//
// Which node types the graph may name is not a setting: it is the engine's
// "node_type" deps. The engine therefore holds no collaborator of its own and a
// deployment cannot configure a type it did not mount.
type Settings struct {
	// Graph is the graph definition: literal content, {"file": ...}, or
	// {"embed": ...}. A plain string is literal content.
	Graph json.RawMessage `json:"graph"`
	Build BuildSettings   `json:"build,omitempty"`
}

// BuildSettings mirrors the graph Build options as JSON.
type BuildSettings struct {
	MaxIterations        *int              `json:"max_iterations,omitempty"`
	Timeout              *string           `json:"timeout,omitempty"`
	RunEndPublishTimeout *string           `json:"run_end_publish_timeout,omitempty"`
	MaxNodeRetries       *int              `json:"max_node_retries,omitempty"`
	Parallel             *ParallelSettings `json:"parallel,omitempty"`
}

// ParallelSettings mirrors [coregraph.ParallelConfig] as JSON.
type ParallelSettings struct {
	Enabled        bool    `json:"enabled,omitempty"`
	BranchTimeout  *string `json:"branch_timeout,omitempty"`
	MaxConcurrency *int    `json:"max_concurrency,omitempty"`
	MaxBranches    *int    `json:"max_branches,omitempty"`
	MergeStrategy  *string `json:"merge_strategy,omitempty"`
}

// Factory builds an agent.Engine/graph: it compiles a
// [coregraph.GraphDefinition] into a [coregraph.Graph] with the node types
// mounted as "node_type" deps.
type Factory struct{}

// Spec implements res.Factory. Both deps are optional and neither is a
// collaborator: "node_type" carries every type the graph may name (the engine
// registers no default one), "script_bindings" replaces the script surface for
// the whole graph.
func (Factory) Spec() res.Spec {
	return res.Spec{
		Kind: "agent.Engine",
		Impl: "graph",
		Deps: []res.Requirement{
			depNodeTypes.Spec(),
			depBindings.Spec(),
		},
	}
}

// New implements res.Factory.
func (Factory) New(ctx context.Context, in res.Input) (*coregraph.Graph, error) {
	settings, err := res.DecodeTyped[Settings](ctx, in.Settings)
	if err != nil {
		return nil, errdefs.Validationf("graph engine: decode settings: %w", err)
	}
	if len(settings.Graph) == 0 {
		return nil, errdefs.Validationf("graph engine: settings.graph is required")
	}
	src, err := res.ParseSource(settings.Graph)
	if err != nil {
		return nil, errdefs.Validationf("graph engine: settings.graph: %w", err)
	}
	customs, err := collectNodeTypes(in)
	if err != nil {
		return nil, err
	}
	definition, err := loadDefinition(ctx, in, src, mergeFileRefFields(customs))
	if err != nil {
		return nil, err
	}

	registry := coregraph.NewRegistry()
	// Every type comes from a dep, and registration errors name the dep
	// they came from: with no default type there is no "the engine's own"
	// registration left to blame.
	for _, custom := range customs {
		if err := custom.registrar.Register(registry); err != nil {
			return nil, fmt.Errorf("graph engine: node type dep %q: %w", custom.depKey, err)
		}
	}

	options, err := settings.Build.options()
	if err != nil {
		return nil, err
	}
	// The script surface is engine-level and optional: a wired
	// "script_bindings" dep serves every script execution of this
	// graph. Without it each script-running node type falls back to the
	// standard bindings over its own deps — the pre-provider behaviour.
	bindingsProvider, _, err := depBindings.TryGet(in)
	if err != nil {
		return nil, err
	}
	if bindingsProvider != nil {
		options = append(options, coregraph.WithScriptBindings(bindingsProvider))
	}
	built, err := coregraph.Build(definition, registry, options...)
	if err != nil {
		return nil, err
	}
	logBuildWarnings(ctx, definition.Name, built.Warnings())
	return built, nil
}

// logBuildWarnings surfaces the graph's non-fatal topology findings
// (unreachable nodes, unguarded cycles, missing default branches, …).
// Build never fails on them, so without this the deployment path —
// the only production caller of the factory — dropped them on the
// floor: a graph whose sole exit is the iteration guard deployed
// silently and only surfaced as a budget error under load.
func logBuildWarnings(ctx context.Context, name string, warnings []coregraph.Warning) {
	for _, w := range warnings {
		telemetry.Warn(ctx, "graph build warning",
			attribute.String(telemetry.AttrGraphName, name),
			attribute.String("graph.warning.kind", string(w.Kind)),
			attribute.String(telemetry.AttrNodeID, w.NodeID),
			attribute.String("graph.warning.message", w.Message),
		)
	}
}

// Register adds the graph engine factory and the node type resources this
// package ships to r.
//
// A deployment that wants the standard graph names mounts the types it uses;
// nothing is registered into a graph by default.
func Register(r *res.Registry) error {
	for _, register := range []func(*res.Registry) error{
		func(r *res.Registry) error { return res.Register(r, Factory{}) },
		func(r *res.Registry) error { return res.Register(r, InferenceNodeTypeFactory{}) },
		func(r *res.Registry) error { return res.Register(r, ToolNodeTypeFactory{}) },
		func(r *res.Registry) error { return res.Register(r, ScriptNodeTypeFactory{}) },
		func(r *res.Registry) error { return res.Register(r, ScriptInlineNodeTypeFactory{}) },
		func(r *res.Registry) error { return res.Register(r, ScriptBindingsFactory{}) },
		func(r *res.Registry) error { return res.Register(r, ScriptBindingsNoneFactory{}) },
		func(r *res.Registry) error { return res.Register(r, ScriptBindingsChainFactory{}) },
	} {
		if err := register(r); err != nil {
			return err
		}
	}
	return nil
}

func loadDefinition(
	ctx context.Context,
	in res.Input,
	src res.Source,
	customFileFields map[string][]string,
) (*coregraph.GraphDefinition, error) {
	data, err := resolveSource(ctx, in, src)
	if err != nil {
		return nil, err
	}
	definition, err := decode.Decode[coregraph.GraphDefinition](data)
	if err != nil {
		return nil, errdefs.Validationf("graph engine: decode definition: %w", err)
	}
	if err := materializeConfigFileRefs(ctx, in, &definition, customFileFields); err != nil {
		return nil, err
	}
	return &definition, nil
}

// resolveSource materializes a source reference through the input's
// loader; inline content is returned as-is and never needs a loader.
func resolveSource(ctx context.Context, in res.Input, src res.Source) ([]byte, error) {
	if !src.IsRef() {
		return src.Inline, nil
	}
	if in.Loader == nil {
		return nil, errdefs.Validationf(
			"graph engine: source resolution is not configured")
	}
	return in.Loader.Load(ctx, src)
}

// materializeConfigFileRefs rewrites every node config field that a mounted node
// type declared as a source-reference carrier (coregraph.ConfigFileRefFields)
// into its inline content. Each type answers for its own fields, so this stays a
// generic pass: the engine no longer knows that a "script" node has a source or
// an "inference" node a system_prompt.
func materializeConfigFileRefs(
	ctx context.Context,
	in res.Input,
	definition *coregraph.GraphDefinition,
	fileRefFields map[string][]string,
) error {
	for index := range definition.Nodes {
		node := &definition.Nodes[index]
		if len(node.Config) == 0 {
			continue
		}
		var configFields map[string]json.RawMessage
		if err := json.Unmarshal(node.Config, &configFields); err != nil {
			return errdefs.Validationf(
				"graph engine: node %q config: %w", node.ID, err)
		}
		changed := false
		for _, field := range fileRefFields[node.Type] {
			raw, ok := configFields[field]
			if !ok || len(raw) == 0 || raw[0] != '{' {
				continue
			}
			src, err := res.ParseSource(raw)
			if err != nil {
				return errdefs.Validationf(
					"graph engine: node %q config.%s: %w", node.ID, field, err)
			}
			if !src.IsRef() {
				continue
			}
			data, err := resolveSource(ctx, in, src)
			if err != nil {
				return fmt.Errorf(
					"graph engine: node %q config.%s: %w", node.ID, field, err)
			}
			content, err := json.Marshal(string(data))
			if err != nil {
				return errdefs.Internalf(
					"graph engine: node %q config.%s: %w", node.ID, field, err)
			}
			configFields[field] = content
			changed = true
		}
		if changed {
			merged, err := json.Marshal(configFields)
			if err != nil {
				return errdefs.Internalf(
					"graph engine: node %q config: %w", node.ID, err)
			}
			node.Config = merged
		}
	}
	return nil
}

// capabilities is the optional collaborator set a script-running node type or
// the standard script surface accepts. Each factory declares the subset it knows
// how to use; a capability that is absent leaves the matching script global
// unregistered, so calls on it fail closed rather than reaching an unwired
// collaborator.
type capabilities struct {
	inference *inference.Assembly
	router    *route.Router
	tools     *tool.Assembly
	workspace workspace.Workspace
	sandbox   sandbox.Runner
}

// decodeCapabilities reads the optional collaborator handles. Key validation (an
// undeclared dep, a missing required one, a producer of the wrong type) is the
// deployment plan's job, so a document error is reported once, before any
// factory runs.
func decodeCapabilities(in res.Input) (capabilities, error) {
	var out capabilities
	var err error
	if out.inference, _, err = depInference.TryGet(in); err != nil {
		return out, err
	}
	if out.router, _, err = depRouter.TryGet(in); err != nil {
		return out, err
	}
	if out.tools, _, err = depTools.TryGet(in); err != nil {
		return out, err
	}
	if out.workspace, _, err = depWorkspace.TryGet(in); err != nil {
		return out, err
	}
	if out.sandbox, _, err = depSandbox.TryGet(in); err != nil {
		return out, err
	}
	return out, nil
}

// scriptRuntimes is the script runtime set a node type was wired with, split by
// how the dep key spelled the name a node config selects.
type scriptRuntimes struct {
	// named holds "script_runtime.<name>" deps, keyed by <name>.
	named map[string]agent.ScriptRuntime
	// bare holds a runtime mounted as "script_runtime" with no name. A
	// fixed-source type can use it (nothing selects a name); the generic
	// script type cannot, because its node config must name a runtime.
	bare agent.ScriptRuntime
}

// decodeScriptRuntimes splits the "script_runtime" Many dep into named and
// unsuffixed entries.
func decodeScriptRuntimes(in res.Input) (scriptRuntimes, error) {
	values, err := depScriptRuntimes.GetAllNamed(in)
	if err != nil {
		return scriptRuntimes{}, err
	}
	out := scriptRuntimes{named: make(map[string]agent.ScriptRuntime, len(values))}
	for _, value := range values {
		if value.Name == DepScriptRuntime {
			out.bare = value.Value
			continue
		}
		out.named[strings.TrimPrefix(value.Name, DepScriptRuntime+".")] = value.Value
	}
	return out, nil
}

// pick resolves the runtime a fixed-source node type executes on: the configured
// name, or the only wired runtime.
func (r scriptRuntimes) pick(name string) (agent.ScriptRuntime, error) {
	if name != "" {
		if runtime, ok := r.named[name]; ok {
			return runtime, nil
		}
		return nil, fmt.Errorf("runtime %q is not wired (mounted: %s)", name, r.list())
	}
	if len(r.named) == 1 && r.bare == nil {
		for _, runtime := range r.named {
			return runtime, nil
		}
	}
	if len(r.named) == 0 && r.bare != nil {
		return r.bare, nil
	}
	return nil, fmt.Errorf(
		"several script runtimes are wired (%s); set settings.runtime to select one", r.list())
}

// list renders the wired runtimes for a diagnostic, in a stable order.
func (r scriptRuntimes) list() string {
	names := make([]string, 0, len(r.named)+1)
	for name := range r.named {
		names = append(names, name)
	}
	if r.bare != nil {
		names = append(names, DepScriptRuntime)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

type customNodeType struct {
	registrar coregraph.NodeTypeRegistrar
	// depKey is the "node_type.<suffix>" key the resource was mounted
	// under, kept so every diagnostic can point at the document entry
	// rather than at a Go value the author never wrote.
	depKey string
}

// collectNodeTypes validates the engine's Many "node_type" deps and returns them
// in deterministic (sorted key) order, each with the dep key it came from.
//
// It rejects a node type resource mounted under more than one dep key, and — for
// registrars that can report their type names up front
// ([coregraph.TypeNameProvider]) — two resources claiming one type name, naming
// both dep keys. A registrar that cannot report names still collides, but the
// error then comes from RegisterType with only the dep key that lost.
func collectNodeTypes(in res.Input) ([]customNodeType, error) {
	values, err := depNodeTypes.GetAllNamed(in)
	if err != nil {
		return nil, err
	}

	out := make([]customNodeType, 0, len(values))
	mounted := make(map[string]string)   // registrar identity -> first dep key
	providers := make(map[string]string) // node type name -> dep key
	for _, named := range values {
		registrar := named.Value
		if identity, has := registrarIdentity(registrar); has {
			if first, dup := mounted[identity]; dup {
				return nil, errdefs.Conflictf(
					"graph engine: node type resource mounted twice (%s and %s)",
					first, named.Name)
			}
			mounted[identity] = named.Name
		}
		if provider, ok := registrar.(coregraph.TypeNameProvider); ok {
			for _, typeName := range provider.NodeTypeNames() {
				if first, dup := providers[typeName]; dup {
					return nil, errdefs.Conflictf(
						"graph engine: node type %q is provided by both dep %q and dep %q",
						typeName, first, named.Name)
				}
				providers[typeName] = named.Name
			}
		}
		out = append(out, customNodeType{registrar: registrar, depKey: named.Name})
	}
	return out, nil
}

// registrarIdentity returns a stable identity for pointer-backed
// registrar values, used to detect the same node type resource being
// mounted under multiple dep keys. Non-pointer values (structs) have
// no identity and fall back to duplicate-type-name detection at
// registration time.
func registrarIdentity(value any) (string, bool) {
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map,
		reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return fmt.Sprintf("%s:%d", rv.Type(), rv.Pointer()), true
	default:
		return "", false
	}
}

// mergeFileRefFields collects, per node type name, the config fields that may
// carry structured source references. Every type answers for its own fields via
// [coregraph.ConfigFileRefFields] — including the standard "inference" and
// "script" types, which are resources like any other — so the engine keeps no
// table of its own.
func mergeFileRefFields(customs []customNodeType) map[string][]string {
	merged := make(map[string][]string)
	for _, custom := range customs {
		fields, ok := custom.registrar.(coregraph.ConfigFileRefFields)
		if !ok {
			continue
		}
		for typeName, names := range fields.FileRefFields() {
			merged[typeName] = append(merged[typeName], names...)
		}
	}
	return merged
}

func (b BuildSettings) options() ([]coregraph.BuildOption, error) {
	var options []coregraph.BuildOption
	if b.MaxIterations != nil {
		if *b.MaxIterations < 0 {
			return nil, errdefs.Validationf("graph engine: build.max_iterations must be >= 0")
		}
		// 0 is meaningful, not "unset": it lifts the loop guard. The
		// option must therefore be applied whenever the key is present
		// — an absent key keeps the engine default (100).
		options = append(options, coregraph.WithMaxIterations(*b.MaxIterations))
	}
	if b.Timeout != nil {
		timeout, err := parseDuration("build.timeout", *b.Timeout)
		if err != nil {
			return nil, err
		}
		options = append(options, coregraph.WithTimeout(timeout))
	}
	if b.RunEndPublishTimeout != nil {
		timeout, err := parseDuration("build.run_end_publish_timeout", *b.RunEndPublishTimeout)
		if err != nil {
			return nil, err
		}
		if timeout == 0 {
			return nil, errdefs.Validationf(
				"graph engine: build.run_end_publish_timeout must be > 0")
		}
		options = append(options, coregraph.WithRunEndPublishTimeout(timeout))
	}
	if b.MaxNodeRetries != nil {
		if *b.MaxNodeRetries < 0 {
			return nil, errdefs.Validationf("graph engine: build.max_node_retries must be >= 0")
		}
		options = append(options, coregraph.WithMaxNodeRetries(*b.MaxNodeRetries))
	}
	if b.Parallel != nil {
		parallel, err := b.Parallel.config()
		if err != nil {
			return nil, err
		}
		options = append(options, coregraph.WithParallel(parallel))
	}
	return options, nil
}

func (p ParallelSettings) config() (coregraph.ParallelConfig, error) {
	out := coregraph.ParallelConfig{Enabled: p.Enabled}
	if p.BranchTimeout != nil {
		duration, err := parseDuration("build.parallel.branch_timeout", *p.BranchTimeout)
		if err != nil {
			return out, err
		}
		out.BranchTimeout = duration
	}
	if p.MaxConcurrency != nil {
		if *p.MaxConcurrency < 0 {
			return out, errdefs.Validationf("graph engine: build.parallel.max_concurrency must be >= 0")
		}
		out.MaxConcurrency = *p.MaxConcurrency
	}
	if p.MaxBranches != nil {
		if *p.MaxBranches < 0 {
			return out, errdefs.Validationf("graph engine: build.parallel.max_branches must be >= 0")
		}
		out.MaxBranches = *p.MaxBranches
	}
	if p.MergeStrategy != nil {
		out.MergeStrategy = coregraph.MergeStrategy(*p.MergeStrategy)
		switch out.MergeStrategy {
		case "", coregraph.FirstWriteWins, coregraph.LastWriteWins:
		default:
			return out, errdefs.Validationf(
				"graph engine: build.parallel.merge_strategy %q is not built in", *p.MergeStrategy)
		}
	}
	return out, nil
}

func parseDuration(field, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, errdefs.Validation(fmt.Errorf("graph engine: %s: %w", field, err))
	}
	if duration < 0 {
		return 0, errdefs.Validationf("graph engine: %s must be >= 0", field)
	}
	return duration, nil
}
