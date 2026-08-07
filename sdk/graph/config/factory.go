// Package config assembles graph agents: it provides the production
// agent.Factory for sdk/graph and is the graph domain's deployment
// configuration assembly.
package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	coregraph "github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/nodes"
	scriptnode "github.com/GizClaw/flowcraft/sdk/graph/nodes/script"
	inferenceconfig "github.com/GizClaw/flowcraft/sdk/inference/config"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	toolconfig "github.com/GizClaw/flowcraft/sdk/tool/config"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const (
	// Kind is the agent registry key for graph engines.
	Kind = "graph"

	// Stable dependency names used by deployment documents.
	DepInference     = "inference"
	DepTools         = "tools"
	DepWorkspace     = "workspace"
	DepSandbox       = "sandbox"
	DepScriptRuntime = "script_runtime"

	// MaxDefinitionBytes bounds a graph definition loaded from a file.
	MaxDefinitionBytes = 1 << 20

	defaultScriptRuntimeName = "js"
)

// FileLoader loads one graph definition. Implementations must honor ctx.
type FileLoader func(ctx context.Context, path string) ([]byte, error)

// Option configures a Factory.
type Option func(*Factory)

// WithFileLoader replaces the default bounded OS file loader.
func WithFileLoader(loader FileLoader) Option {
	return func(f *Factory) {
		if loader != nil {
			f.loader = loader
		}
	}
}

// WithFS loads files from fsys. Paths are slash-normalized and must remain
// relative to the filesystem root.
func WithFS(fsys fs.FS) Option {
	return func(f *Factory) {
		if fsys == nil {
			return
		}
		f.loader = func(ctx context.Context, path string) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			name := filepath.ToSlash(path)
			name = strings.TrimPrefix(name, "./")
			if !fs.ValidPath(name) {
				return nil, &fs.PathError{Op: "read", Path: path, Err: fs.ErrInvalid}
			}
			file, err := fsys.Open(name)
			if err != nil {
				return nil, err
			}
			defer func() { _ = file.Close() }()
			if err := requireRegularFile(file, path); err != nil {
				return nil, err
			}
			data, err := readBounded(file)
			if err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return data, nil
		}
	}
}

// WithBaseDir sets the base directory used to resolve relative graph files.
func WithBaseDir(dir string) Option {
	return func(f *Factory) {
		if dir != "" {
			f.baseDir = filepath.Clean(dir)
		}
	}
}

// Factory builds independent sdk/graph engines.
type Factory struct {
	loader  FileLoader
	baseDir string
}

// NewFactory returns a graph agent factory.
func NewFactory(options ...Option) *Factory {
	f := &Factory{
		loader:  loadOSFile,
		baseDir: ".",
	}
	for _, option := range options {
		if option != nil {
			option(f)
		}
	}
	return f
}

// Spec implements agent.Factory.
func (*Factory) Spec() agent.EngineSpec {
	return agent.EngineSpec{
		Kind: Kind,
		Capabilities: agent.Capabilities{
			SupportsResume:  true,
			EmitsCheckpoint: true,
			EmitsUserPrompt: true,
		},
		Deps: []agent.DepSpec{
			{Name: DepInference, Type: "inference.Assembly"},
			{Name: DepTools, Type: toolconfig.ResourceKind},
			{Name: DepWorkspace, Type: "workspace.Workspace"},
			{Name: DepSandbox, Type: "sandbox.Runner"},
			{Name: DepScriptRuntime, Type: "agent.ScriptRuntime"},
		},
	}
}

type settings struct {
	Graph             *graphSource  `json:"graph"`
	ScriptRuntimeName string        `json:"script_runtime_name,omitempty"`
	Build             buildSettings `json:"build,omitempty"`
}

type graphSource struct {
	File   string          `json:"file,omitempty"`
	Inline json.RawMessage `json:"inline,omitempty"`
}

func (s *graphSource) UnmarshalJSON(data []byte) error {
	if len(bytes.TrimSpace(data)) > 0 && bytes.TrimSpace(data)[0] == '"' {
		return json.Unmarshal(data, &s.File)
	}
	type graphSourceWire graphSource
	var wire graphSourceWire
	if err := decodeStrictJSON(data, &wire); err != nil {
		return err
	}
	*s = graphSource(wire)
	return nil
}

type buildSettings struct {
	MaxIterations        *int              `json:"max_iterations,omitempty"`
	Timeout              *string           `json:"timeout,omitempty"`
	RunEndPublishTimeout *string           `json:"run_end_publish_timeout,omitempty"`
	MaxNodeRetries       *int              `json:"max_node_retries,omitempty"`
	Parallel             *parallelSettings `json:"parallel,omitempty"`
}

type parallelSettings struct {
	Enabled        bool    `json:"enabled,omitempty"`
	BranchTimeout  *string `json:"branch_timeout,omitempty"`
	MaxConcurrency *int    `json:"max_concurrency,omitempty"`
	MaxBranches    *int    `json:"max_branches,omitempty"`
	MergeStrategy  *string `json:"merge_strategy,omitempty"`
}

type dependencies struct {
	inference *inferenceconfig.Assembly
	tools     *toolconfig.Assembly
	workspace workspace.Workspace
	sandbox   sandbox.Runner
	script    agent.ScriptRuntime
}

func (d dependencies) validate() error {
	if d.inference != nil && d.inference.Runtime == nil {
		return errdefs.Validationf(
			"graph agent: dep %q has a nil Runtime", DepInference)
	}
	if d.tools != nil {
		if d.tools.Executor == nil {
			return errdefs.Validationf(
				"graph agent: dep %q has a nil Executor", DepTools)
		}
		if isNil(d.tools.Catalog) {
			return errdefs.Validationf(
				"graph agent: dep %q has a nil Catalog", DepTools)
		}
	}
	return nil
}

// New implements agent.Factory.
func (f *Factory) New(ctx context.Context, cfg agent.Config) (agent.Engine, error) {
	if f == nil {
		return nil, errdefs.Validationf("graph agent factory is nil")
	}
	parsed, err := decodeSettings(cfg.Settings)
	if err != nil {
		return nil, err
	}
	definition, err := f.definition(ctx, parsed.Graph)
	if err != nil {
		return nil, err
	}
	deps, err := decodeDependencies(cfg.Deps)
	if err != nil {
		return nil, err
	}
	if err := deps.validate(); err != nil {
		return nil, err
	}
	runtimeName := parsed.ScriptRuntimeName
	if runtimeName == "" {
		runtimeName = defaultScriptRuntimeName
	}
	required, err := scanNodeTypes(definition)
	if err != nil {
		return nil, err
	}
	if err := validateRequiredDeps(required, deps); err != nil {
		return nil, err
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
		inferenceDeps.Runtime = deps.inference.Runtime
		inferenceDeps.Router = deps.inference.Router
		scriptDeps.InferenceRuntime = deps.inference.Runtime
		scriptDeps.InferenceRouter = deps.inference.Router
	}
	if deps.tools != nil {
		inferenceDeps.Catalog = deps.tools.Catalog
		scriptDeps.ToolDispatcher = deps.tools.Executor
		scriptDeps.ToolCatalog = deps.tools.Catalog
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

	options, err := parsed.Build.options()
	if err != nil {
		return nil, err
	}
	return coregraph.Build(definition, registry, options...)
}

func decodeSettings(raw map[string]any) (settings, error) {
	var out settings
	data, err := json.Marshal(raw)
	if err != nil {
		return out, errdefs.Validation(fmt.Errorf("graph agent settings: encode: %w", err))
	}
	if err := decodeStrictJSON(data, &out); err != nil {
		return out, errdefs.Validation(fmt.Errorf("graph agent settings: %w", err))
	}
	if out.Graph == nil {
		return out, errdefs.Validationf("graph agent settings: graph is required")
	}
	return out, nil
}

func (f *Factory) definition(ctx context.Context, source *graphSource) (*coregraph.GraphDefinition, error) {
	hasFile := source.File != ""
	hasInline := len(source.Inline) != 0 && !bytes.Equal(bytes.TrimSpace(source.Inline), []byte("null"))
	if hasFile == hasInline {
		return nil, errdefs.Validationf("graph agent settings: exactly one of graph.file or graph.inline is required")
	}
	var data []byte
	if hasFile {
		path, err := f.resolvePath(source.File)
		if err != nil {
			return nil, err
		}
		loader := f.loader
		if loader == nil {
			loader = loadOSFile
		}
		data, err = loader(ctx, path)
		if err != nil {
			switch {
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				return nil, errdefs.FromContext(err)
			case errdefs.HasClassification(err):
				return nil, fmt.Errorf("graph agent: read definition %q: %w", path, err)
			case errors.Is(err, fs.ErrNotExist):
				return nil, errdefs.NotFound(fmt.Errorf("graph agent: read definition %q: %w", path, err))
			case errors.Is(err, fs.ErrInvalid):
				return nil, errdefs.Validation(fmt.Errorf("graph agent: read definition %q: %w", path, err))
			case errors.Is(err, fs.ErrPermission):
				return nil, errdefs.Forbidden(fmt.Errorf("graph agent: read definition %q: %w", path, err))
			default:
				return nil, errdefs.Internal(fmt.Errorf("graph agent: read definition %q: %w", path, err))
			}
		}
	} else {
		data = source.Inline
	}
	if len(data) > MaxDefinitionBytes {
		return nil, errdefs.Validationf(
			"graph agent: definition exceeds %d bytes", MaxDefinitionBytes)
	}
	var definition coregraph.GraphDefinition
	if err := decodeStrictJSON(data, &definition); err != nil {
		return nil, errdefs.Validation(fmt.Errorf("graph agent: decode definition: %w", err))
	}
	if err := f.materializeConfigFileRefs(ctx, &definition); err != nil {
		return nil, err
	}
	return &definition, nil
}

// configFileFields returns the node-config field names that may carry
// a structured {"file": ...} reference for a node type. The factory
// materializes those references into inline values before the graph
// kernel builds, mirroring how the graph source itself splits
// file/inline forms. The kernel stays filesystem-free and only ever
// sees fully materialized node configs.
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

// materializeConfigFileRefs resolves structured file references in
// node configs (e.g. "source": {"file": "scripts/run.js"}) into inline
// values using the factory's loader and base directory. Plain string
// values are left untouched; malformed reference objects are rejected
// so typos surface at build time.
func (f *Factory) materializeConfigFileRefs(ctx context.Context, definition *coregraph.GraphDefinition) error {
	for index := range definition.Nodes {
		node := &definition.Nodes[index]
		if len(node.Config) == 0 {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(node.Config, &fields); err != nil {
			return errdefs.Validationf(
				"graph agent: node %q config: %v", node.ID, err)
		}
		changed := false
		for _, field := range configFileFields(node.Type) {
			raw, ok := fields[field]
			if !ok || len(raw) == 0 || raw[0] != '{' {
				continue
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(raw, &object); err != nil {
				return errdefs.Validationf(
					"graph agent: node %q config.%s: %v", node.ID, field, err)
			}
			if len(object) != 1 || object["file"] == nil {
				return errdefs.Validationf(
					"graph agent: node %q config.%s: expected a string or {\"file\": \"...\"}, got %s",
					node.ID, field, raw)
			}
			var ref struct {
				File string `json:"file"`
			}
			if err := json.Unmarshal(raw, &ref); err != nil {
				return errdefs.Validationf(
					"graph agent: node %q config.%s: %v", node.ID, field, err)
			}
			ref.File = strings.TrimSpace(ref.File)
			if ref.File == "" {
				return errdefs.Validationf(
					"graph agent: node %q config.%s.file must be non-empty", node.ID, field)
			}
			path, err := f.resolvePath(ref.File)
			if err != nil {
				return err
			}
			loader := f.loader
			if loader == nil {
				loader = loadOSFile
			}
			data, err := loader(ctx, path)
			if err != nil {
				switch {
				case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
					return errdefs.FromContext(err)
				case errdefs.HasClassification(err):
					return err
				case errors.Is(err, fs.ErrNotExist):
					return errdefs.NotFound(fmt.Errorf(
						"graph agent: node %q config.%s file %q: %w", node.ID, field, ref.File, err))
				default:
					return errdefs.Internal(fmt.Errorf(
						"graph agent: node %q config.%s file %q: %w", node.ID, field, ref.File, err))
				}
			}
			content, err := json.Marshal(string(data))
			if err != nil {
				return errdefs.Internalf(
					"graph agent: node %q config.%s: %v", node.ID, field, err)
			}
			fields[field] = content
			changed = true
		}
		if changed {
			merged, err := json.Marshal(fields)
			if err != nil {
				return errdefs.Internalf(
					"graph agent: node %q config: %v", node.ID, err)
			}
			node.Config = merged
		}
	}
	return nil
}

func (f *Factory) resolvePath(name string) (string, error) {
	clean := filepath.Clean(name)
	if filepath.IsAbs(clean) {
		return clean, nil
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errdefs.Validationf("graph agent: graph.file %q escapes base directory", name)
	}
	return filepath.Join(f.baseDir, clean), nil
}

func loadOSFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	if err := requireRegularFile(file, path); err != nil {
		return nil, err
	}
	data, err := readBounded(file)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func readBounded(reader io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(reader, MaxDefinitionBytes+1))
}

func requireRegularFile(file fs.File, path string) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return &fs.PathError{Op: "read", Path: path, Err: fs.ErrInvalid}
	}
	return nil
}

func decodeStrictJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

func decodeDependencies(raw map[string]any) (dependencies, error) {
	var out dependencies
	known := map[string]bool{
		DepInference: true, DepTools: true, DepWorkspace: true,
		DepSandbox: true, DepScriptRuntime: true,
	}
	for name := range raw {
		if !known[name] {
			return out, errdefs.Validationf("graph agent: unknown dep %q", name)
		}
	}
	var err error
	if out.inference, err = optionalDep[*inferenceconfig.Assembly](raw, DepInference); err != nil {
		return out, err
	}
	if out.tools, err = optionalDep[*toolconfig.Assembly](raw, DepTools); err != nil {
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
			"graph agent: dep %q has Go type %T, want %v", name, value, reflect.TypeFor[T]())
	}
	if isNil(typed) {
		return zero, errdefs.Validationf("graph agent: dep %q is a typed nil", name)
	}
	return typed, nil
}

func isNil(value any) bool {
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
					"graph agent: inference node %q config: %w", node.ID, err)
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
	tools, ok := fields["tools"]
	if !ok || bytes.Equal(bytes.TrimSpace(tools), []byte("null")) {
		return modelConfigured, nil, false, nil
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
	return modelConfigured, staticTools, len(names) > 0, nil
}

func validateRequiredDeps(required nodeRequirements, deps dependencies) error {
	switch {
	case required.inference && deps.inference == nil:
		return errdefs.NotFoundf("graph agent: node type inference requires dep %q", DepInference)
	case required.tools && deps.tools == nil:
		return errdefs.NotFoundf("graph agent: node type tool requires dep %q", DepTools)
	case required.script && deps.script == nil:
		return errdefs.NotFoundf("graph agent: node type script requires dep %q", DepScriptRuntime)
	case required.router && deps.inference.Router == nil:
		return errdefs.NotFoundf(
			"graph agent: inference node without model requires a Router in dep %q",
			DepInference)
	default:
		return validateToolNames(required.toolNames, deps.tools)
	}
}

func validateToolNames(names []string, assembly *toolconfig.Assembly) error {
	if len(names) == 0 {
		return nil
	}
	available := make(map[string]struct{})
	for _, definition := range assembly.Catalog.Definitions() {
		available[definition.Name] = struct{}{}
	}
	for _, name := range names {
		if _, ok := available[name]; !ok {
			return errdefs.NotFoundf(
				"graph agent: inference node references unknown tool %q", name)
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
				"graph agent: script node %q runtime %q does not match bound runtime %q",
				node.ID, config.Runtime, boundName)
		}
	}
	return nil
}

func (b buildSettings) options() ([]coregraph.BuildOption, error) {
	var options []coregraph.BuildOption
	if b.MaxIterations != nil {
		if *b.MaxIterations < 0 {
			return nil, errdefs.Validationf("graph agent: build.max_iterations must be >= 0")
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
		timeout, err := parseDuration(
			"build.run_end_publish_timeout",
			*b.RunEndPublishTimeout,
		)
		if err != nil {
			return nil, err
		}
		if timeout == 0 {
			return nil, errdefs.Validationf(
				"graph agent: build.run_end_publish_timeout must be > 0")
		}
		options = append(options, coregraph.WithRunEndPublishTimeout(timeout))
	}
	if b.MaxNodeRetries != nil {
		if *b.MaxNodeRetries < 0 {
			return nil, errdefs.Validationf("graph agent: build.max_node_retries must be >= 0")
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

func (p parallelSettings) config() (coregraph.ParallelConfig, error) {
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
			return out, errdefs.Validationf("graph agent: build.parallel.max_concurrency must be >= 0")
		}
		out.MaxConcurrency = *p.MaxConcurrency
	}
	if p.MaxBranches != nil {
		if *p.MaxBranches < 0 {
			return out, errdefs.Validationf("graph agent: build.parallel.max_branches must be >= 0")
		}
		out.MaxBranches = *p.MaxBranches
	}
	if p.MergeStrategy != nil {
		out.MergeStrategy = coregraph.MergeStrategy(*p.MergeStrategy)
		switch out.MergeStrategy {
		case "", coregraph.FirstWriteWins, coregraph.LastWriteWins:
		default:
			return out, errdefs.Validationf(
				"graph agent: build.parallel.merge_strategy %q is not built in", *p.MergeStrategy)
		}
	}
	return out, nil
}

func parseDuration(field, value string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, errdefs.Validation(fmt.Errorf("graph agent: %s: %w", field, err))
	}
	if duration < 0 {
		return 0, errdefs.Validationf("graph agent: %s must be >= 0", field)
	}
	return duration, nil
}

var _ agent.Factory = (*Factory)(nil)
