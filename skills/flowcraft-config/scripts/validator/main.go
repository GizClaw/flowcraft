// Command validate-config is the L2 validator for FlowCraft
// configuration. Its default entry is a deployment document (any
// filename); --type switches to a standalone sub-document entry
// (inference, workspace, sandbox, tool, graph, agent).
//
// The deploy path parses every sub-document and dry-builds the
// deployment through the real sdkx/deploy assembly layer with
// first-party factories and stub secrets. The validator is hermetic: no
// network calls, no real credentials, no remote engines. App-registered
// kinds, sources, and hook types are reported as scope limits instead of
// being guessed at.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/agent"
	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	sdkconfigutils "github.com/GizClaw/flowcraft/sdk/config/utils"
	eventconfig "github.com/GizClaw/flowcraft/sdk/event/config"
	"github.com/GizClaw/flowcraft/sdk/graph"
	graphconfig "github.com/GizClaw/flowcraft/sdk/graph/config"
	"github.com/GizClaw/flowcraft/sdk/graph/nodes"
	scriptnode "github.com/GizClaw/flowcraft/sdk/graph/nodes/script"
	inferenceconfig "github.com/GizClaw/flowcraft/sdk/inference/config"
	sandboxconfig "github.com/GizClaw/flowcraft/sdk/sandbox/config"
	schedulerconfig "github.com/GizClaw/flowcraft/sdk/scheduler/config"
	"github.com/GizClaw/flowcraft/sdk/tool"
	toolconfig "github.com/GizClaw/flowcraft/sdk/tool/config"
	workspaceconfig "github.com/GizClaw/flowcraft/sdk/workspace/config"
	sqliteconfig "github.com/GizClaw/flowcraft/sdkx/agent/checkpoint/sqlite/config"
	jsrt "github.com/GizClaw/flowcraft/sdkx/agent/script/jsrt"
	luart "github.com/GizClaw/flowcraft/sdkx/agent/script/luart"
	"github.com/GizClaw/flowcraft/sdkx/delegation"
	delegationconfig "github.com/GizClaw/flowcraft/sdkx/delegation/config"
	kanbanconfig "github.com/GizClaw/flowcraft/sdkx/delegation/kanban/config"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/inference/anthropic"
	"github.com/GizClaw/flowcraft/sdkx/inference/azure"
	"github.com/GizClaw/flowcraft/sdkx/inference/bytedance"
	"github.com/GizClaw/flowcraft/sdkx/inference/deepseek"
	"github.com/GizClaw/flowcraft/sdkx/inference/kimi"
	"github.com/GizClaw/flowcraft/sdkx/inference/minimax"
	"github.com/GizClaw/flowcraft/sdkx/inference/openai"
	"github.com/GizClaw/flowcraft/sdkx/inference/qwen"
	memoryhook "github.com/GizClaw/flowcraft/sdkx/memory/hook"
	runtimecore "github.com/GizClaw/flowcraft/sdkx/runtime"
	sdkscheduler "github.com/GizClaw/flowcraft/sdkx/scheduler"
)

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

var resolverPattern = regexp.MustCompile(`(?m)^\s*resolver\s*:\s*([A-Za-z0-9_-]+)\s*$`)

// stubEngine lets L2 validate an a2a engine's wiring without fetching a
// remote agent card. Execution always fails closed.
type stubEngine struct{}

func (stubEngine) Execute(ctx context.Context, run agent.Run, host agent.Host, board *agent.Board) (*agent.Board, error) {
	return nil, fmt.Errorf("a2a engine stub: remote execution is not performed during config validation")
}

// stubA2AFactory keeps the a2a settings subtree strict-decoded at the
// top level while skipping remote card resolution.
type stubA2AFactory struct{}

func (stubA2AFactory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{Kind: "a2a"}
}

func (stubA2AFactory) New(_ context.Context, in sdkconfig.Input) (any, error) {
	if len(in.Settings) > 0 {
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(in.Settings, &probe); err != nil {
			return nil, fmt.Errorf("a2a: settings must be a JSON object: %w", err)
		}
	}
	return stubEngine{}, nil
}

// stubCheckpointFactory mirrors the sqlite checkpoint factory's settings
// validation without opening a database file.
type stubCheckpointFactory struct{}

func (stubCheckpointFactory) Spec() sdkconfig.Spec {
	return sdkconfig.Spec{Kind: "agent.CheckpointStore", Impl: "sqlite"}
}

func (stubCheckpointFactory) New(_ context.Context, in sdkconfig.Input) (any, error) {
	settings, err := sdkconfig.DecodeSettings[sqliteconfig.Settings](in.Settings)
	if err != nil {
		return nil, fmt.Errorf("sqlite checkpoint config: decode settings: %w", err)
	}
	if strings.TrimSpace(settings.Path) == "" {
		return nil, fmt.Errorf("sqlite checkpoint config: settings.path is required")
	}
	return agent.NoopCheckpointStore{}, nil
}

func stubResolve(_ context.Context, key string) (inferenceconfig.Secret, error) {
	return inferenceconfig.NewSecret([]byte("stub-" + key))
}

func main() {
	os.Exit(run())
}

func run() int {
	baseDir := flag.String("base-dir", "", "base directory for relative config paths (default: the input file's directory)")
	typ := flag.String("type", "deploy", "document type: deploy, inference, workspace, sandbox, tool, graph, agent")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: validate-config [--base-dir DIR] [--type TYPE] <config-file>\n")
		fmt.Fprintln(os.Stderr, "types: deploy (default), inference, workspace, sandbox, tool, graph, agent")
		flag.PrintDefaults()
	}
	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		flag.Usage()
		return exitUsage
	}
	path := args[0]
	dir := *baseDir
	if dir == "" {
		dir = filepath.Dir(path)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "base-dir: %v\n", err)
		return exitUsage
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", path, err)
		return exitError
	}

	loader := sdkconfig.NewLoader(sdkconfig.WithBaseDir(absDir))
	ctx := context.Background()

	switch *typ {
	case "deploy":
		return validateDeploy(raw, path, loader, ctx)
	case "inference":
		return validateInference(raw, loader, ctx)
	case "workspace":
		return validateWorkspace(raw, absDir, ctx)
	case "sandbox":
		return validateSandbox(raw)
	case "tool":
		return validateTool(raw, ctx)
	case "graph":
		return validateGraphFile(raw, path, loader, ctx)
	case "agent":
		return validateAgentFile(raw)
	default:
		fmt.Fprintf(os.Stderr, "unknown --type %q (want deploy, inference, workspace, sandbox, tool, graph, or agent)\n", *typ)
		return exitUsage
	}
}

func validateDeploy(raw []byte, deployPath string, loader *sdkconfig.Loader, ctx context.Context) int {
	doc, err := deploy.Parse(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[parse] %s\n", err)
		return exitError
	}

	scopeNotes := scanScope(doc)
	resolvers := discoverResolvers(loader, ctx, doc, raw)
	if err := validateGraphNodeConfigs(loader, ctx, doc); err != nil {
		fmt.Fprintf(os.Stderr, "[graph] %s\n", err)
		return exitError
	}

	var runtimeRefs []string
	if doc.Runtime != nil {
		refs, err := preflightRuntime(doc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[runtime] %s\n", err)
			return exitError
		}
		runtimeRefs = refs
	}

	builder := deploy.NewBuilder(deploy.WithLoader(loader))
	registerBuilder(builder, loader, resolvers)

	result, err := builder.Build(ctx, doc, deploy.WithExternalResourceConsumers(runtimeRefs...))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[build] %s\n", err)
		printBuildGuidance(os.Stderr, err)
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "L2 scope notes:")
		for _, note := range scopeNotes {
			fmt.Fprintf(os.Stderr, "  - %s\n", note)
		}
		fmt.Fprintf(os.Stderr, "stub secret resolvers: %s\n", strings.Join(sortedKeys(resolvers), ", "))
		return exitError
	}
	defer result.Close()

	var warnings []string
	warnings = append(warnings, scopeNotes...)
	if doc.Runtime != nil {
		if err := validateRuntime(doc, result, &warnings); err != nil {
			fmt.Fprintf(os.Stderr, "[runtime] %s\n", err)
			return exitError
		}
	}
	warnings = append(warnings, checkToolAllowLists(result, doc)...)

	fmt.Printf("OK: %s\n", deployPath)
	fmt.Printf("resources: %s\n", strings.Join(result.ResourceNames(), ", "))
	fmt.Printf("agents: %s\n", strings.Join(result.InstanceNames(), ", "))
	if doc.Runtime != nil {
		fmt.Println("runtime: present (validated)")
	} else {
		fmt.Println("runtime: absent (deploy-only document)")
	}
	if len(warnings) > 0 {
		fmt.Println("warnings:")
		for _, w := range warnings {
			fmt.Printf("  - %s\n", w)
		}
	}
	return exitOK
}

func validateInference(raw []byte, loader *sdkconfig.Loader, ctx context.Context) int {
	doc, err := inferenceconfig.Parse(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[parse] inference config: %s\n", err)
		return exitError
	}
	builder, err := inferenceconfig.NewBuilder(providerFactories(), stubResolvers(resolverNamesFrom(raw)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[build] inference config: %s\n", err)
		return exitError
	}
	asm, err := builder.NewAssembly(ctx, doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[build] inference config: %s\n", err)
		printBuildGuidance(os.Stderr, err)
		return exitError
	}
	router := "none"
	if asm.Router != nil {
		router = "present"
	}
	fmt.Printf("OK: inference config (%d providers, route router %s)\n", len(doc.Providers), router)
	return exitOK
}

func validateWorkspace(raw []byte, baseDir string, ctx context.Context) int {
	doc, err := workspaceconfig.Parse(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[parse] workspace config: %s\n", err)
		return exitError
	}
	registry, err := workspaceconfig.NewBuilder(workspaceconfig.Deps{BaseDir: baseDir}).Build(ctx, doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[build] workspace config: %s\n", err)
		printBuildGuidance(os.Stderr, err)
		return exitError
	}
	defer registry.Close()
	names := sortedNames(doc.Workspaces)
	fmt.Printf("OK: workspace config (%d workspaces: %s)\n", len(names), strings.Join(names, ", "))
	return exitOK
}

func validateSandbox(raw []byte) int {
	doc, err := sandboxconfig.Parse(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[parse] sandbox config: %s\n", err)
		return exitError
	}
	names := sortedNames(doc.Sandboxes)
	fmt.Printf("OK: sandbox config (%d sandboxes: %s)\n", len(names), strings.Join(names, ", "))
	fmt.Println("note: structural validation only; full build requires the workspace registry dep in a deploy document (sandbox.Registry deps.workspaces)")
	return exitOK
}

func validateTool(raw []byte, ctx context.Context) int {
	doc, err := toolconfig.Parse(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[parse] tool config: %s\n", err)
		return exitError
	}
	asm, err := toolconfig.NewBuilder(toolconfig.Deps{}).Build(ctx, doc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[build] tool config: %s\n", err)
		printBuildGuidance(os.Stderr, err)
		return exitError
	}
	defer asm.Close()
	defs := asm.Catalog.Definitions()
	fmt.Printf("OK: tool config (%d middlewares, %d tools in catalog)\n", len(doc.Middlewares), len(defs))
	return exitOK
}

func validateGraphFile(raw []byte, path string, loader *sdkconfig.Loader, ctx context.Context) int {
	def, err := sdkconfigutils.Decode[graph.GraphDefinition](raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[parse] graph definition: %s\n", err)
		return exitError
	}
	if err := def.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "[parse] graph definition: %s\n", err)
		return exitError
	}
	if err := validateGraphNodes(loader, ctx, def); err != nil {
		fmt.Fprintf(os.Stderr, "[graph] %s\n", err)
		return exitError
	}
	fmt.Printf("OK: graph %q (version %q, %d nodes, %d edges)\n",
		def.Name, def.Version, len(def.Nodes), len(def.Edges))
	return exitOK
}

// validateAgentFile mirrors sdkx/deploy's parseAgentFile and
// validateAgentEntry for a standalone agent recipe.
func validateAgentFile(raw []byte) int {
	probe, err := sdkconfigutils.Decode[map[string]json.RawMessage](raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[parse] agent document: %s\n", err)
		return exitError
	}
	versionRaw, ok := probe["version"]
	if !ok {
		fmt.Fprintln(os.Stderr, "[parse] agent document: agent config version is required")
		return exitError
	}
	var version string
	if err := json.Unmarshal(versionRaw, &version); err != nil {
		fmt.Fprintf(os.Stderr, "[parse] agent document: decode version: %s\n", err)
		return exitError
	}
	if version != deploy.VersionV1 {
		fmt.Fprintf(os.Stderr, "[parse] agent document: agent config version %q is not supported (want %q)\n", version, deploy.VersionV1)
		return exitError
	}
	if _, ok := probe["source"]; ok {
		fmt.Fprintln(os.Stderr, "[parse] agent document: field source is not allowed in an agent file")
		return exitError
	}
	delete(probe, "version")
	body, err := json.Marshal(probe)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[parse] agent document: re-encode: %s\n", err)
		return exitError
	}
	var entry deploy.AgentEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		fmt.Fprintf(os.Stderr, "[parse] agent document: %s\n", err)
		return exitError
	}
	if entry.Engine.Kind == "" {
		fmt.Fprintln(os.Stderr, "[parse] agent document: engine.kind is required")
		return exitError
	}
	if err := validateAgentDeps(entry.Deps); err != nil {
		fmt.Fprintf(os.Stderr, "[parse] agent document: %s\n", err)
		return exitError
	}
	for _, h := range entry.Prepare {
		if err := validateHook(h.Type, h.Deps); err != nil {
			fmt.Fprintf(os.Stderr, "[parse] agent document: prepare: %s\n", err)
			return exitError
		}
	}
	for _, h := range entry.Observe {
		if err := validateHook(h.Type, h.Deps); err != nil {
			fmt.Fprintf(os.Stderr, "[parse] agent document: observe: %s\n", err)
			return exitError
		}
	}
	for _, h := range entry.Referees {
		if err := validateHook(h.Type, h.Deps); err != nil {
			fmt.Fprintf(os.Stderr, "[parse] agent document: referees: %s\n", err)
			return exitError
		}
	}
	for _, h := range entry.Commit {
		if err := validateHook(h.Type, h.Deps); err != nil {
			fmt.Fprintf(os.Stderr, "[parse] agent document: commit: %s\n", err)
			return exitError
		}
	}
	if entry.Policy.MaxRevise < 0 {
		fmt.Fprintln(os.Stderr, "[parse] agent document: policy.max_revise must be >= 0")
		return exitError
	}
	fmt.Printf("OK: agent recipe (engine %q, %d deps, %d hooks)\n",
		entry.Engine.Kind, len(entry.Deps),
		len(entry.Prepare)+len(entry.Observe)+len(entry.Referees)+len(entry.Commit))
	return exitOK
}

func validateHook(hookType string, deps map[string]deploy.DepRef) error {
	if hookType == "" {
		return fmt.Errorf("type is required")
	}
	return validateAgentDeps(deps)
}

func validateAgentDeps(deps map[string]deploy.DepRef) error {
	for name, dep := range deps {
		if name == "" {
			return fmt.Errorf("deps: empty dep name")
		}
		switch {
		case dep.Source == "" && dep.Resource == "":
			return fmt.Errorf("deps[%q]: source or resource is required", name)
		case dep.Source != "" && dep.Resource != "":
			return fmt.Errorf("deps[%q]: source and resource are mutually exclusive", name)
		}
	}
	return nil
}

// registerBuilder registers every first-party factory the L2 validator
// can construct hermetically. App-registered kinds remain out of scope.
func registerBuilder(builder *deploy.Builder, loader *sdkconfig.Loader, resolvers map[string]bool) {
	builder.RegisterEngine(graphconfig.NewFactory(graphconfig.WithLoader(loader)))
	builder.RegisterEngine(stubA2AFactory{})

	workspaceBuilder := workspaceconfig.NewBuilder(workspaceconfig.Deps{})
	builder.MustRegisterResource(workspaceBuilder)
	sandboxBuilder := sandboxconfig.NewBuilder(sandboxconfig.Deps{})
	builder.MustRegisterResource(sandboxBuilder)

	toolBuilder := toolconfig.NewBuilder(toolconfig.Deps{})
	builder.MustRegisterResource(toolBuilder)

	builder.MustRegisterResource(inferenceconfig.NewDeployFactory(providerFactories(), stubResolvers(resolvers)))

	builder.MustRegisterResource(eventconfig.NewMemoryDeployFactory())
	builder.MustRegisterResource(jsrt.NewDeployFactory())
	builder.MustRegisterResource(luart.NewDeployFactory())

	schedulerBuilder := schedulerconfig.NewBuilder()
	_ = sdkscheduler.Register(schedulerBuilder)
	builder.MustRegisterResource(schedulerconfig.NewDeployFactory("local", schedulerBuilder))
	builder.MustRegisterResource(kanbanconfig.NewMemoryDeployFactory())
	builder.MustRegisterResource(stubCheckpointFactory{})

	builder.RegisterPreparer(memoryhook.ContextType, memoryhook.ContextPreparer{})
	builder.RegisterCommitter(memoryhook.TurnType, memoryhook.TurnCommitter{})
	builder.RegisterReferee(delegationconfig.RefereeType,
		delegationconfig.NewHandoffRefereeFactory(delegation.NewDirectory()))
}

func providerFactories() map[string]inferenceconfig.Factory {
	return map[string]inferenceconfig.Factory{
		"openai":    openai.Factory(),
		"azure":     azure.Factory(),
		"deepseek":  deepseek.Factory(),
		"qwen":      qwen.Factory(),
		"bytedance": bytedance.Factory(),
		"minimax":   minimax.Factory(),
		"kimi":      kimi.Factory(),
		"anthropic": anthropic.Factory(),
	}
}

func stubResolvers(extra map[string]bool) map[string]inferenceconfig.SecretResolver {
	resolvers := map[string]inferenceconfig.SecretResolver{
		"env":  inferenceconfig.SecretResolverFunc(stubResolve),
		"file": inferenceconfig.SecretResolverFunc(stubResolve),
	}
	for name := range extra {
		if _, ok := resolvers[name]; !ok {
			resolvers[name] = inferenceconfig.SecretResolverFunc(stubResolve)
		}
	}
	return resolvers
}

// resolverNamesFrom finds every secret resolver name referenced in one
// document's bytes.
func resolverNamesFrom(data []byte) map[string]bool {
	names := map[string]bool{}
	for _, match := range resolverPattern.FindAllSubmatch(data, -1) {
		names[string(match[1])] = true
	}
	return names
}

// discoverResolvers finds every secret resolver name referenced anywhere
// in the deploy document and its file-backed resource documents, so L2
// can stub them without reading real credentials.
func discoverResolvers(loader *sdkconfig.Loader, ctx context.Context, doc deploy.Document, raw []byte) map[string]bool {
	names := resolverNamesFrom(raw)
	for _, entry := range doc.Resources {
		if len(entry.Settings) == 0 {
			continue
		}
		var src sdkconfig.Source
		if err := json.Unmarshal(entry.Settings, &src); err != nil {
			continue
		}
		switch src.Kind() {
		case sdkconfig.SourceFile:
			data, err := loader.Load(ctx, src)
			if err == nil {
				for name := range resolverNamesFrom(data) {
					names[name] = true
				}
			}
		case sdkconfig.SourceContent:
			if content, ok := src.Content(); ok {
				for name := range resolverNamesFrom(content) {
					names[name] = true
				}
			}
		case sdkconfig.SourceLiteral:
			if literal, ok := src.Literal(); ok {
				for name := range resolverNamesFrom([]byte(literal)) {
					names[name] = true
				}
			}
		}
	}
	return names
}

// scanScope lists document surfaces the L2 validator cannot construct:
// app-registered kinds, impls, engines, hooks, and host sources.
func scanScope(doc deploy.Document) []string {
	var notes []string
	knownKinds := map[string]bool{
		"workspace.Registry":      true,
		"sandbox.Registry":        true,
		"inference.Assembly":      true,
		"tool.Assembly":           true,
		"event.Bus":               true,
		"agent.ScriptRuntime":     true,
		"scheduler.Server":        true,
		"delegation.AsyncBackend": true,
		"agent.CheckpointStore":   true,
	}
	knownImpls := map[string]bool{
		"workspace.Registry/yaml":               true,
		"sandbox.Registry/yaml":                 true,
		"inference.Assembly/yaml":               true,
		"tool.Assembly/yaml":                    true,
		"event.Bus/memory":                      true,
		"agent.ScriptRuntime/js":                true,
		"agent.ScriptRuntime/lua":               true,
		"scheduler.Server/local":                true,
		"delegation.AsyncBackend/kanban-memory": true,
		"agent.CheckpointStore/sqlite":          true,
	}
	for name, entry := range doc.Resources {
		if !knownKinds[entry.Kind] {
			notes = append(notes, fmt.Sprintf(
				"resource %q kind %q is not first-party; L2 cannot build it (verify your app registers it)", name, entry.Kind))
			continue
		}
		if !knownImpls[entry.Kind+"/"+entry.Impl] {
			notes = append(notes, fmt.Sprintf(
				"resource %q impl %q is not first-party; L2 cannot build it", name, entry.Impl))
		}
	}
	for name, entry := range doc.Agents {
		if entry.Engine.Kind != "graph" && entry.Engine.Kind != "a2a" {
			notes = append(notes, fmt.Sprintf(
				"agent %q engine kind %q is not first-party; L2 cannot build it", name, entry.Engine.Kind))
		}
		for _, dep := range entry.Deps {
			if dep.Source != "" {
				notes = append(notes, fmt.Sprintf(
					"agent %q binds host source %q; L2 cannot resolve it", name, dep.Source))
			}
		}
		for _, h := range entry.Prepare {
			if h.Type != memoryhook.ContextType {
				notes = append(notes, fmt.Sprintf(
					"agent %q prepare hook %q is app-registered; L2 cannot build it", name, h.Type))
			}
		}
		for _, h := range entry.Referees {
			if h.Type != "discard_on_interrupt" && h.Type != delegationconfig.RefereeType {
				notes = append(notes, fmt.Sprintf(
					"agent %q referee %q is app-registered; L2 cannot build it", name, h.Type))
			}
		}
		for _, h := range entry.Commit {
			if h.Type != memoryhook.TurnType {
				notes = append(notes, fmt.Sprintf(
					"agent %q commit hook %q is app-registered; L2 cannot build it", name, h.Type))
			}
		}
		for _, h := range entry.Observe {
			notes = append(notes, fmt.Sprintf(
				"agent %q observer %q is app-registered; L2 cannot build it", name, h.Type))
		}
	}
	return notes
}

func preflightRuntime(doc deploy.Document) ([]string, error) {
	cfg, err := runtimecore.DecodeConfig(doc)
	if err != nil {
		return nil, err
	}
	var refs []string
	for _, ref := range []string{cfg.EventBus, cfg.Scheduler, cfg.CheckpointStore} {
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	for _, item := range cfg.Integrations {
		for _, ref := range item.Deps {
			refs = append(refs, ref)
		}
	}
	var missing []string
	for _, ref := range refs {
		if _, ok := doc.Resources[ref]; !ok {
			missing = append(missing, ref)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("runtime references resources that are not declared in the deployment: %s", strings.Join(missing, ", "))
	}
	return refs, nil
}

func validateRuntime(doc deploy.Document, result *deploy.Result, warnings *[]string) error {
	cfg, err := runtimecore.DecodeConfig(doc)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, name := range result.ResourceNames() {
		names[name] = true
	}
	var refs []string
	for _, ref := range []string{cfg.EventBus, cfg.Scheduler, cfg.CheckpointStore} {
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	for _, item := range cfg.Integrations {
		for _, ref := range item.Deps {
			refs = append(refs, ref)
		}
	}
	var missing []string
	for _, ref := range refs {
		if !names[ref] {
			missing = append(missing, ref)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("runtime references resources that are not in the deployment: %s", strings.Join(missing, ", "))
	}
	if cfg.Sessions.Resume && cfg.CheckpointStore == "" {
		return fmt.Errorf("runtime sessions.resume requires checkpoint_store")
	}
	*warnings = append(*warnings,
		fmt.Sprintf("runtime validated: event_bus=%s scheduler=%s checkpoint_store=%s sessions.resume=%v integrations=%d",
			cfg.EventBus, cfg.Scheduler, cfg.CheckpointStore, cfg.Sessions.Resume, len(cfg.Integrations)))
	return nil
}

// validateGraphNodeConfigs statically decodes every node config in every
// graph definition referenced by a graph engine. The deploy build only
// checks top-level node config field names; this pass catches semantic
// errors (bad model refs, missing script fields) that otherwise surface
// only at invocation time.
func validateGraphNodeConfigs(loader *sdkconfig.Loader, ctx context.Context, doc deploy.Document) error {
	for agentName, entry := range doc.Agents {
		if entry.Engine.Kind != "graph" || len(entry.Engine.Settings) == 0 {
			continue
		}
		var engineSettings struct {
			Graph json.RawMessage `json:"graph"`
		}
		if err := json.Unmarshal(entry.Engine.Settings, &engineSettings); err != nil {
			return fmt.Errorf("agent %q: engine settings must be a JSON object", agentName)
		}
		if len(engineSettings.Graph) == 0 {
			return fmt.Errorf("agent %q: graph engine settings require a graph source", agentName)
		}
		var src sdkconfig.Source
		if err := json.Unmarshal(engineSettings.Graph, &src); err != nil {
			return fmt.Errorf("agent %q: graph source: %w", agentName, err)
		}
		data, err := loader.Load(ctx, src)
		if err != nil {
			return fmt.Errorf("agent %q: graph source: %w", agentName, err)
		}
		def, err := sdkconfigutils.Decode[graph.GraphDefinition](data)
		if err != nil {
			return fmt.Errorf("agent %q: graph definition: %w", agentName, err)
		}
		if err := def.Validate(); err != nil {
			return fmt.Errorf("agent %q: graph definition: %w", agentName, err)
		}
		if err := validateGraphNodes(loader, ctx, def); err != nil {
			return fmt.Errorf("agent %q: %w", agentName, err)
		}
	}
	return nil
}

func validateGraphNodes(loader *sdkconfig.Loader, ctx context.Context, def graph.GraphDefinition) error {
	for i := range def.Nodes {
		node := &def.Nodes[i]
		if len(node.Config) == 0 {
			continue
		}
		config, err := materializeNodeConfigRefs(loader, ctx, node)
		if err != nil {
			return fmt.Errorf("graph %q node %q: %w", def.Name, node.ID, err)
		}
		switch node.Type {
		case "inference":
			cfg, err := graph.DecodeConfig[nodes.InferenceConfig](config)
			if err != nil {
				return fmt.Errorf("graph %q node %q (inference): %w", def.Name, node.ID, err)
			}
			if cfg.Model != nil {
				if err := cfg.Model.Validate(); err != nil {
					return fmt.Errorf("graph %q node %q (inference): model: %w", def.Name, node.ID, err)
				}
			}
		case "tool":
			if _, err := graph.DecodeConfig[nodes.ToolConfig](config); err != nil {
				return fmt.Errorf("graph %q node %q (tool): %w", def.Name, node.ID, err)
			}
		case "script":
			cfg, err := graph.DecodeConfig[scriptnode.ScriptConfig](config)
			if err != nil {
				return fmt.Errorf("graph %q node %q (script): %w", def.Name, node.ID, err)
			}
			if cfg.Runtime == "" {
				return fmt.Errorf("graph %q node %q (script): runtime is required", def.Name, node.ID)
			}
			if cfg.Source == "" {
				return fmt.Errorf("graph %q node %q (script): source is required", def.Name, node.ID)
			}
		default:
			return fmt.Errorf("graph %q node %q: unknown node type %q (first-party types: inference, tool, script)", def.Name, node.ID, node.Type)
		}
	}
	return nil
}

// materializeNodeConfigRefs resolves structured {"file": ...} /
// {"embed": ...} references in the node-config fields the graph factory
// materializes (script.source, inference.system_prompt), mirroring the
// factory so strict decoding sees the same values the kernel would.
func materializeNodeConfigRefs(loader *sdkconfig.Loader, ctx context.Context, node *graph.NodeDefinition) (json.RawMessage, error) {
	fields := map[string]bool{}
	switch node.Type {
	case "script":
		fields["source"] = true
	case "inference":
		fields["system_prompt"] = true
	}
	if len(fields) == 0 || len(node.Config) == 0 {
		return node.Config, nil
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(node.Config, &config); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	changed := false
	for field := range fields {
		raw, ok := config[field]
		if !ok || len(raw) == 0 || raw[0] != '{' {
			continue
		}
		var src sdkconfig.Source
		if err := json.Unmarshal(raw, &src); err != nil {
			return nil, fmt.Errorf("config.%s: %w", field, err)
		}
		data, err := loader.Load(ctx, src)
		if err != nil {
			return nil, fmt.Errorf("config.%s: %w", field, err)
		}
		content, err := json.Marshal(string(data))
		if err != nil {
			return nil, fmt.Errorf("config.%s: %w", field, err)
		}
		config[field] = content
		changed = true
	}
	if !changed {
		return node.Config, nil
	}
	return json.Marshal(config)
}

func checkToolAllowLists(result *deploy.Result, doc deploy.Document) []string {
	var catalogs []tool.Catalog
	for _, name := range result.ResourceNames() {
		asm, err := deploy.ResourceAs[*toolconfig.Assembly](result, name)
		if err == nil && asm != nil && asm.Catalog != nil {
			catalogs = append(catalogs, asm.Catalog)
		}
	}
	var warnings []string
	for agentName, entry := range doc.Agents {
		for _, toolName := range entry.Tools {
			found := false
			for _, catalog := range catalogs {
				if _, ok := catalog.Get(toolName); ok {
					found = true
					break
				}
			}
			if !found {
				warnings = append(warnings, fmt.Sprintf(
					"agent %q allow-lists tool %q but no built tool catalog in this document exposes it (may be app-registered)", agentName, toolName))
			}
		}
	}
	return warnings
}

func printBuildGuidance(out *os.File, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "is not registered"):
		fmt.Fprintln(out, "guidance: an unregistered kind/impl/engine/hook usually means a typo in the document, or an app-registered component L2 cannot know about. Check the L2 scope notes.")
	case strings.Contains(msg, "provider driver"):
		fmt.Fprintln(out, "guidance: the provider driver is not one of the first-party drivers L2 registers (openai, azure, deepseek, qwen, bytedance, minimax, kimi, anthropic).")
	case strings.Contains(msg, "secret resolver"):
		fmt.Fprintln(out, "guidance: the secret resolver is not registered. L2 stubs every resolver name it can discover from the document; app-owned resolvers must be registered by the host.")
	case strings.Contains(msg, "builtin tool"):
		fmt.Fprintln(out, "guidance: tools.yaml names a builtin the host must register on the tool builder (RegisterBuiltin); L2 cannot know app-registered tools.")
	case strings.Contains(msg, "graph agent"):
		fmt.Fprintln(out, "guidance: the graph definition or its node config failed. Check node ids, types, config keys, edges, and engine dep wiring.")
	case strings.Contains(msg, "memory config"):
		fmt.Fprintln(out, "guidance: memory implementation wiring failed. Check the implementation module's schema and that hooks bind the whole memory.Assembly.")
	case strings.Contains(msg, "dead configuration"):
		fmt.Fprintln(out, "guidance: a built resource is consumed by nothing. Bind it from an agent/hook/another resource, set export: true, or remove it.")
	case strings.Contains(msg, "inference config"):
		fmt.Fprintln(out, "guidance: the inference document or its route targets failed. Check provider ids/drivers, secret refs, and that route models exist in the provider catalog.")
	case strings.Contains(msg, "workspace config"):
		fmt.Fprintln(out, "guidance: the workspace document or driver settings failed. Check drivers, roots, and scope pattern rules.")
	case strings.Contains(msg, "tool config"):
		fmt.Fprintln(out, "guidance: the tool document failed. Check middleware kinds/specs, source kinds, and scopes (agent/platform).")
	case strings.Contains(msg, "sandbox config"):
		fmt.Fprintln(out, "guidance: the sandbox document failed. Check backends, workspace names, defaults, and approval config.")
	}
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedNames[V any](set map[string]V) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
