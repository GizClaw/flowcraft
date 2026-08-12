// Package resource adapts the graph engine to deployment resources:
// it registers the agent.Engine/graph factory that compiles a
// [graph.GraphDefinition] with node types wired from resource deps.
package resource

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	coregraph "github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/core/graph/nodes"
	scriptnode "github.com/GizClaw/flowcraft/core/graph/nodes/script"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/route"
	res "github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/utils"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// Stable dependency names used by deployment documents.
const (
	DepInference     = "inference"
	DepRouter        = "router"
	DepTools         = "tools"
	DepWorkspace     = "workspace"
	DepSandbox       = "sandbox"
	DepScriptRuntime = "script_runtime"

	defaultScriptRuntimeName = "js"
)

// Settings is the strict settings subtree of the graph engine factory:
// the graph definition source plus build knobs.
type Settings struct {
	// Graph is the graph definition: literal content, {"file": ...}, or
	// {"embed": ...}. A plain string is literal content.
	Graph             json.RawMessage `json:"graph"`
	ScriptRuntimeName string          `json:"script_runtime_name,omitempty"`
	Build             BuildSettings   `json:"build,omitempty"`
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
// [coregraph.GraphDefinition] into a [coregraph.Graph] with the node
// types wired from its deps.
type Factory struct{}

// Spec implements res.Factory. Every dep is optional; the requirements
// are derived from the graph definition itself (an inference node
// requires inference or router, a tool node requires tools, a script
// node requires script_runtime).
func (Factory) Spec() res.Spec {
	return res.Spec{
		Kind: "agent.Engine",
		Impl: "graph",
		Deps: []res.DepSpec{
			{Name: DepInference, Type: "inference.Assembly"},
			{Name: DepRouter, Type: "inference.Router"},
			{Name: DepTools, Type: "tool.Assembly"},
			{Name: DepWorkspace, Type: "workspace.Workspace"},
			{Name: DepSandbox, Type: "sandbox.Runner"},
			{Name: DepScriptRuntime, Type: "agent.ScriptRuntime"},
		},
	}
}

// New implements res.Factory.
func (Factory) New(ctx context.Context, in res.Input) (any, error) {
	settings, err := res.DecodeTyped[Settings](in.Settings)
	if err != nil {
		return nil, errdefs.Validationf("graph engine: decode settings: %v", err)
	}
	if len(settings.Graph) == 0 {
		return nil, errdefs.Validationf("graph engine: settings.graph is required")
	}
	src, err := res.ParseSource(settings.Graph)
	if err != nil {
		return nil, errdefs.Validationf("graph engine: settings.graph: %v", err)
	}
	definition, err := loadDefinition(ctx, in, src)
	if err != nil {
		return nil, err
	}
	deps, err := decodeDependencies(in.Deps)
	if err != nil {
		return nil, err
	}
	required, err := scanNodeTypes(definition)
	if err != nil {
		return nil, err
	}
	if err := validateRequiredDeps(required, deps); err != nil {
		return nil, err
	}
	runtimeName := settings.ScriptRuntimeName
	if runtimeName == "" {
		runtimeName = defaultScriptRuntimeName
	}
	if err := validateScriptRuntimes(definition, runtimeName); err != nil {
		return nil, err
	}

	registry := coregraph.NewRegistry()
	inferenceDeps := nodes.InferenceNodeDeps{}
	scriptDeps := scriptnode.ScriptNodeDeps{
		Runtimes:      make(map[string]agent.ScriptRuntime),
		Workspace:     deps.workspace,
		CommandRunner: deps.sandbox,
	}
	if deps.inference != nil {
		inferenceDeps.Assembly = deps.inference
		scriptDeps.InferenceAssembly = deps.inference
	}
	if deps.router != nil {
		inferenceDeps.Router = deps.router
		scriptDeps.InferenceRouter = deps.router
	}
	if deps.tools != nil {
		inferenceDeps.Catalog = deps.tools.Catalog()
		scriptDeps.ToolDispatcher = deps.tools
		scriptDeps.ToolCatalog = deps.tools.Catalog()
	}
	if deps.script != nil {
		scriptDeps.Runtimes[runtimeName] = deps.script
	}
	if err := nodes.RegisterInference(registry, inferenceDeps); err != nil {
		return nil, err
	}
	if err := nodes.RegisterTool(registry, scriptDeps.ToolDispatcher); err != nil {
		return nil, err
	}
	if err := scriptnode.Register(registry, scriptDeps); err != nil {
		return nil, err
	}

	options, err := settings.Build.options()
	if err != nil {
		return nil, err
	}
	return coregraph.Build(definition, registry, options...)
}

// Register adds the graph engine factory to r.
func Register(r *res.Registry) error {
	return r.Register(Factory{})
}

func loadDefinition(ctx context.Context, in res.Input, src res.Source) (*coregraph.GraphDefinition, error) {
	data, err := resolveSource(ctx, in, src)
	if err != nil {
		return nil, err
	}
	definition, err := utils.Decode[coregraph.GraphDefinition](data)
	if err != nil {
		return nil, errdefs.Validationf("graph engine: decode definition: %v", err)
	}
	if err := materializeConfigFileRefs(ctx, in, &definition); err != nil {
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

// configFileFields returns the node-config field names that may carry
// a structured {"file": ...} reference for a node type. The factory
// materializes those references into inline values before the graph
// kernel builds, so the kernel stays filesystem-free.
func configFileFields(nodeType string) []string {
	switch nodeType {
	case "script":
		return []string{"source"}
	case "inference":
		return []string{"system_prompt"}
	default:
		return nil
	}
}

func materializeConfigFileRefs(ctx context.Context, in res.Input, definition *coregraph.GraphDefinition) error {
	for index := range definition.Nodes {
		node := &definition.Nodes[index]
		if len(node.Config) == 0 {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(node.Config, &fields); err != nil {
			return errdefs.Validationf(
				"graph engine: node %q config: %v", node.ID, err)
		}
		changed := false
		for _, field := range configFileFields(node.Type) {
			raw, ok := fields[field]
			if !ok || len(raw) == 0 || raw[0] != '{' {
				continue
			}
			src, err := res.ParseSource(raw)
			if err != nil {
				return errdefs.Validationf(
					"graph engine: node %q config.%s: %v", node.ID, field, err)
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
					"graph engine: node %q config.%s: %v", node.ID, field, err)
			}
			fields[field] = content
			changed = true
		}
		if changed {
			merged, err := json.Marshal(fields)
			if err != nil {
				return errdefs.Internalf(
					"graph engine: node %q config: %v", node.ID, err)
			}
			node.Config = merged
		}
	}
	return nil
}

type dependencies struct {
	inference *inference.Assembly
	router    *route.Router
	tools     *tool.Assembly
	workspace workspace.Workspace
	sandbox   sandbox.Runner
	script    agent.ScriptRuntime
}

func decodeDependencies(raw map[string]any) (dependencies, error) {
	var out dependencies
	known := map[string]bool{
		DepInference: true, DepRouter: true, DepTools: true,
		DepWorkspace: true, DepSandbox: true, DepScriptRuntime: true,
	}
	for name := range raw {
		if !known[name] {
			return out, errdefs.Validationf("graph engine: unknown dep %q", name)
		}
	}
	var err error
	if out.inference, err = optionalDep[*inference.Assembly](raw, DepInference); err != nil {
		return out, err
	}
	if out.router, err = optionalDep[*route.Router](raw, DepRouter); err != nil {
		return out, err
	}
	if out.tools, err = optionalDep[*tool.Assembly](raw, DepTools); err != nil {
		return out, err
	}
	if out.workspace, err = optionalDep[workspace.Workspace](raw, DepWorkspace); err != nil {
		return out, err
	}
	if out.sandbox, err = optionalDep[sandbox.Runner](raw, DepSandbox); err != nil {
		return out, err
	}
	if out.script, err = optionalDep[agent.ScriptRuntime](raw, DepScriptRuntime); err != nil {
		return out, err
	}
	return out, nil
}

func optionalDep[T any](raw map[string]any, name string) (T, error) {
	var zero T
	value, present := raw[name]
	if !present {
		return zero, nil
	}
	typed, ok := value.(T)
	if !ok {
		return zero, errdefs.Validationf(
			"graph engine: dep %q has Go type %T, want %v", name, value, reflect.TypeFor[T]())
	}
	if isNilValue(typed) {
		return zero, errdefs.Validationf("graph engine: dep %q is a typed nil", name)
	}
	return typed, nil
}

func isNilValue(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

type nodeRequirements struct {
	inference bool
	tools     bool
	script    bool
	router    bool
	toolNames []string
}

func scanNodeTypes(definition *coregraph.GraphDefinition) (nodeRequirements, error) {
	var required nodeRequirements
	for _, node := range definition.Nodes {
		switch node.Type {
		case "inference":
			required.inference = true
			modelConfigured, toolNames, toolsConfigured, err := scanInferenceConfig(node.Config)
			if err != nil {
				return required, fmt.Errorf(
					"graph engine: inference node %q config: %w", node.ID, err)
			}
			required.router = required.router || !modelConfigured
			if toolsConfigured {
				required.tools = true
				required.toolNames = append(required.toolNames, toolNames...)
			}
		case "tool":
			required.tools = true
		case "script":
			required.script = true
		}
	}
	return required, nil
}

func scanInferenceConfig(raw json.RawMessage) (modelConfigured bool, staticTools []string, toolsConfigured bool, err error) {
	var fields map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &fields); err != nil {
			return false, nil, false, errdefs.Validationf(
				"inference config is not a JSON object: %v", err)
		}
	}
	if model, ok := fields["model"]; ok && !bytes.Equal(bytes.TrimSpace(model), []byte("null")) {
		modelConfigured = true
	}
	tools, hasTools := fields["tools"]
	allTools := false
	if rawAll, ok := fields["all_tools"]; ok {
		if err := json.Unmarshal(rawAll, &allTools); err != nil {
			return false, nil, false, errdefs.Validationf(
				"inference config all_tools must be a boolean: %v", err)
		}
	}
	if !hasTools {
		return modelConfigured, nil, allTools, nil
	}
	if bytes.Equal(bytes.TrimSpace(tools), []byte("null")) {
		return modelConfigured, nil, allTools, nil
	}
	var names []string
	if err := json.Unmarshal(tools, &names); err != nil {
		// A board reference may replace the whole value at invocation time.
		return modelConfigured, nil, true, nil
	}
	for _, name := range names {
		if !strings.Contains(name, "${board.") {
			staticTools = append(staticTools, name)
		}
	}
	return modelConfigured, staticTools, len(names) > 0 || allTools, nil
}

func validateRequiredDeps(required nodeRequirements, deps dependencies) error {
	switch {
	case required.inference && deps.inference == nil && deps.router == nil:
		return errdefs.NotFoundf(
			"graph engine: node type inference requires dep %q or %q", DepInference, DepRouter)
	case required.router && deps.router == nil:
		return errdefs.NotFoundf(
			"graph engine: inference node without model requires dep %q", DepRouter)
	case required.tools && deps.tools == nil:
		return errdefs.NotFoundf("graph engine: node type tool requires dep %q", DepTools)
	case required.script && deps.script == nil:
		return errdefs.NotFoundf(
			"graph engine: node type script requires dep %q", DepScriptRuntime)
	default:
		return validateToolNames(required.toolNames, deps.tools)
	}
}

func validateToolNames(names []string, assembly *tool.Assembly) error {
	if len(names) == 0 || assembly == nil {
		return nil
	}
	available := make(map[string]struct{})
	for _, definition := range assembly.Catalog().Definitions() {
		available[definition.Name] = struct{}{}
	}
	for _, name := range names {
		if _, ok := available[name]; !ok {
			return errdefs.NotFoundf(
				"graph engine: inference node references unknown tool %q", name)
		}
	}
	return nil
}

func validateScriptRuntimes(definition *coregraph.GraphDefinition, boundName string) error {
	for _, node := range definition.Nodes {
		if node.Type != "script" {
			continue
		}
		config, err := coregraph.DecodeConfig[scriptnode.ScriptConfig](node.Config)
		if err != nil {
			return err
		}
		if config.Runtime != boundName {
			return errdefs.Validationf(
				"graph engine: script node %q runtime %q does not match bound runtime %q",
				node.ID, config.Runtime, boundName)
		}
	}
	return nil
}

func (b BuildSettings) options() ([]coregraph.BuildOption, error) {
	var options []coregraph.BuildOption
	if b.MaxIterations != nil {
		if *b.MaxIterations < 0 {
			return nil, errdefs.Validationf("graph engine: build.max_iterations must be >= 0")
		}
		if *b.MaxIterations > 0 {
			options = append(options, coregraph.WithMaxIterations(*b.MaxIterations))
		}
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
