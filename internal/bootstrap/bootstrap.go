// Package bootstrap wires all dependencies and returns a ready-to-serve
// *api.Server. Extracted from cmd/flowcraft/ so the wiring logic is
// importable and testable.
package bootstrap

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/api"
	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/internal/gateway"
	"github.com/GizClaw/flowcraft/internal/paths"
	"github.com/GizClaw/flowcraft/internal/realm"
	"github.com/GizClaw/flowcraft/internal/metatool"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/pluginhost"
	"github.com/GizClaw/flowcraft/internal/sandbox"
	"github.com/GizClaw/flowcraft/internal/skill"
	"github.com/GizClaw/flowcraft/internal/store"
	"github.com/GizClaw/flowcraft/internal/template"
	"github.com/GizClaw/flowcraft/internal/version"
	"github.com/GizClaw/flowcraft/plugin"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/adapter"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/executor"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	"github.com/GizClaw/flowcraft/sdk/workflow"
	_ "github.com/GizClaw/flowcraft/sdk/graph/node/scriptnode" // register built-in script node types
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/extract"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"
	"github.com/GizClaw/flowcraft/skills"
	"github.com/GizClaw/flowcraft/web"

	otellog "go.opentelemetry.io/otel/log"
)

// Run wires all dependencies and returns a ready-to-serve HTTP server,
// a cleanup function (call in reverse-init order), and any bootstrap error.
func Run(ctx context.Context) (*api.Server, func(), error) {
	var cleanups []func()
	runCleanups := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	fail := func(err error) (*api.Server, func(), error) {
		runCleanups()
		return nil, nil, err
	}

	if err := paths.EnsureLayout(); err != nil {
		return fail(err)
	}

	cfg := config.Load()
	config.InitLogging(cfg.Log)
	for _, w := range cfg.Validate() {
		telemetry.Warn(ctx, "config validation", otellog.String("warning", w))
	}
	telemetry.Info(ctx, "config loaded",
		otellog.String("address", cfg.Address()),
		otellog.String("configure_path", cfg.ConfigurePath))

	shutdownTracer, shutdownMeter, shutdownLog := initTelemetry(ctx, cfg)
	shutdownTelemetry := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracer(ctx)
		_ = shutdownMeter(ctx)
		_ = shutdownLog(ctx)
	}
	cleanups = append(cleanups, shutdownTelemetry)

	dbPath := cfg.DBPath()
	sqliteStore, err := store.NewSQLiteStore(ctx, dbPath)
	if err != nil {
		telemetry.Error(ctx, "failed to open database",
			otellog.String("path", dbPath), otellog.String("error", err.Error()))
		return fail(err)
	}
	var appStore model.Store = sqliteStore
	if cfg.Telemetry.Enabled {
		appStore = store.WithStoreTracing(appStore)
	}
	cleanups = append(cleanups, func() { _ = appStore.Close() })

	workspaceRoot := cfg.Sandbox.RootDir
	if workspaceRoot == "" {
		workspaceRoot = filepath.Join(cfg.ConfigurePath, "workspace")
	}

	ws, err := workspace.NewLocalWorkspace(workspaceRoot)
	if err != nil {
		telemetry.Error(ctx, "failed to initialize workspace", otellog.String("error", err.Error()))
		return fail(err)
	}

	toolReg := tool.NewRegistry()
	scriptRT := jsrt.New()

	schemaReg := node.NewSchemaRegistry()
	node.RegisterBuiltinSchemas(schemaReg)
	templateReg := template.NewRegistry()
	templateReg.RegisterBuiltins()
	templateReg.SetStore(appStore)
	if err := templateReg.LoadFromStore(ctx); err != nil {
		telemetry.Warn(ctx, "template: load from store failed", otellog.String("error", err.Error()))
	}

	llmResolver := llm.DefaultResolver(appStore)

	fsStore := knowledge.NewFSStore(ws)
	if err := fsStore.BuildIndex(ctx); err != nil {
		telemetry.Warn(ctx, "knowledge: initial index build failed", otellog.String("error", err.Error()))
	}
	semanticLLM, semErr := llmResolver.Resolve(ctx, "")
	knowledgeStore := knowledge.NewCachedStore(fsStore)

	var semProc *knowledge.SemanticProcessor
	if semErr == nil && semanticLLM != nil {
		semProc = knowledge.NewSemanticProcessor(semanticLLM, fsStore,
			knowledge.WithOnEvict(func(datasetID string) { knowledgeStore.EvictDataset(datasetID) }),
		)
		semProc.Start(ctx)
		fsStore.SetSemanticProcessor(semProc)
		cleanups = append(cleanups, func() { semProc.Stop() })
	} else {
		telemetry.Warn(ctx, "knowledge: SemanticProcessor disabled (no LLM configured), L0/L1 summaries will not be generated — knowledge base will use BM25 search only")
	}

	if kw, kwErr := fsStore.StartWatching(ctx); kwErr != nil {
		telemetry.Warn(ctx, "knowledge: file watcher failed to start", otellog.String("error", kwErr.Error()))
	} else if kw != nil {
		cleanups = append(cleanups, func() { kw.Stop() })
	}

	pluginReg := pluginhost.NewRegistry()

	pluginDir := cfg.Plugin.Dir
	if pluginDir == "" {
		pluginDir = filepath.Join(workspaceRoot, "plugins")
	}
	healthInterval := time.Duration(cfg.Plugin.HealthInterval) * time.Second
	if healthInterval <= 0 {
		healthInterval = 10 * time.Second
	}
	maxFailures := cfg.Plugin.MaxFailures
	if maxFailures <= 0 {
		maxFailures = 3
	}
	extMgr := pluginhost.NewExternalManager(pluginhost.ExternalManagerConfig{
		PluginDir:      pluginDir,
		HealthInterval: healthInterval,
		MaxFailures:    maxFailures,
	})
	pluginReg.SetExternalManager(extMgr)

	if cfg.Plugin.ConfigFile != "" {
		if configs, err := pluginhost.LoadPluginsJSON(cfg.Plugin.ConfigFile); err != nil {
			telemetry.Warn(ctx, "plugin: load config failed", otellog.String("error", err.Error()))
		} else {
			for _, pc := range configs {
				if pc.Path != "" && pc.Enabled {
					ep := pluginhost.NewExternalPlugin(pc.Path, plugin.PluginInfo{
						ID:   pc.ID,
						Name: pc.ID,
					})
					_ = pluginReg.RegisterWithConfig(ep, pc.Config)
				}
			}
		}
	}

	_ = pluginReg.InitializeAll(ctx)
	pluginhost.InjectSchemas(pluginReg, schemaReg)

	cleanups = append(cleanups, func() {
		pluginReg.ShutdownAll(context.Background())
		extMgr.Stop(context.Background())
	})

	graphCompiler := compiler.NewCompiler()

	knowledge.RegisterNode(knowledgeStore)
	schemaReg.Register(knowledge.KnowledgeNodeSchema())

	nodeFactory := node.NewFactory(
		node.WithLLMResolver(llmResolver),
		node.WithToolRegistry(toolReg),
		node.WithScriptRuntime(scriptRT),
		node.WithWorkspace(ws),
		node.WithCommandRunner(workspace.NewLocalCommandRunner(workspaceRoot)),
	)

	var sandboxMgr *sandbox.Manager

	jsFallback := nodeFactory.Fallback()
	nodeFactory.SetFallback(func(def graph.NodeDefinition, bctx *node.BuildContext) (graph.Node, error) {
		if p, ok := pluginReg.GetPluginForNodeType(def.Type); ok {
			if ep, ok := p.(*pluginhost.ExternalPlugin); ok {
				resolver := func(pluginID string) (plugin.NodeServiceClient, error) {
					rp, rok := pluginReg.Get(pluginID)
					if !rok {
						return nil, fmt.Errorf("plugin %q not registered", pluginID)
					}
					rep, rok := rp.(*pluginhost.ExternalPlugin)
					if !rok {
						return nil, fmt.Errorf("plugin %q is not external", pluginID)
					}
					client := rep.NodeClient()
					if client == nil {
						return nil, fmt.Errorf("plugin %q has no node service", pluginID)
					}
					return client, nil
				}
				host := &pluginhost.HostCallbackProvider{
					LLMGenerate: func(ctx context.Context, prompt string) (string, error) {
						m, err := llmResolver.Resolve(ctx, "")
						if err != nil {
							return "", fmt.Errorf("plugin llm callback: %w", err)
						}
						resp, _, err := m.Generate(ctx, []llm.Message{llm.NewTextMessage(llm.RoleUser, prompt)})
						if err != nil {
							return "", err
						}
						return resp.Content(), nil
					},
					ToolExecute: func(ctx context.Context, name, args string) (string, error) {
						t, ok := toolReg.Get(name)
						if !ok {
							return "", fmt.Errorf("tool %q not found", name)
						}
						return t.Execute(ctx, args)
					},
					SandboxExec: func(ctx context.Context, command string) (string, error) {
						if handle, ok := model.SandboxHandleFrom(ctx).(*sandbox.SandboxHandle); ok && handle != nil {
							sb, done, err := handle.Acquire(ctx)
							if err != nil {
								return "", fmt.Errorf("plugin sandbox callback: %w", err)
							}
							defer done()
							result, err := sb.Exec(ctx, "sh", []string{"-c", command}, sandbox.ExecOptions{})
							if err != nil {
								return "", err
							}
							return result.Stdout, nil
						}
						if sandboxMgr == nil {
							return "", fmt.Errorf("plugin sandbox callback: sandbox manager not available")
						}
						runtimeID := model.RuntimeIDFrom(ctx)
						if runtimeID == "" {
							return "", fmt.Errorf("plugin sandbox callback: no runtime ID in context")
						}
						sb, err := sandboxMgr.Acquire(ctx, runtimeID, sandbox.AcquireOptions{
							Mode: sandbox.ModePersistent,
						})
						if err != nil {
							return "", fmt.Errorf("plugin sandbox callback: %w", err)
						}
						defer func() { _ = sandboxMgr.Release(runtimeID) }()
						result, err := sb.Exec(ctx, "sh", []string{"-c", command}, sandbox.ExecOptions{})
						if err != nil {
							return "", err
						}
						return result.Stdout, nil
					},
					Signal: func(_ context.Context, _ string, _ any) error {
						return nil
					},
				}
				return pluginhost.NewProxyNode(def.ID, def.Type, ep.Info().ID, def.Config, resolver, host), nil
			}
		}
		if jsFallback != nil {
			return jsFallback(def, bctx)
		}
		return nil, fmt.Errorf("unknown node type %q for node %q", def.Type, def.ID)
	})

	versionStore := version.NewVersionStore(appStore)

	globalBus := event.NewMemoryBus()
	cleanups = append(cleanups, func() { _ = globalBus.Close() })

	sandboxCfg := sandbox.ManagerConfig{
		Driver:        cfg.Sandbox.Driver,
		Mode:          sandbox.ParseMode(cfg.Sandbox.Mode),
		RootDir:       workspaceRoot,
		Image:         cfg.Sandbox.Image,
		MaxConcurrent: cfg.Sandbox.MaxConcurrent,
		NetworkMode:   cfg.Sandbox.NetworkMode,
		CPUQuota:      cfg.Sandbox.CPUQuota,
		MemoryLimit:   cfg.Sandbox.MemoryLimit,
	}
	if sandboxCfg.Driver == "" {
		sandboxCfg.Driver = "local"
	}
	if sandboxCfg.MaxConcurrent <= 0 {
		sandboxCfg.MaxConcurrent = 10
	}
	if cfg.Sandbox.ExecTimeout != "" {
		if d, err := time.ParseDuration(cfg.Sandbox.ExecTimeout); err == nil {
			sandboxCfg.ExecTimeout = d
		}
	}
	if cfg.Sandbox.IdleTimeout != "" {
		if d, err := time.ParseDuration(cfg.Sandbox.IdleTimeout); err == nil {
			sandboxCfg.IdleTimeout = d
		}
	}
	if sandboxCfg.Driver == "docker" {
		sandboxCfg.Mounts = buildSandboxMounts(cfg, workspaceRoot)
	}
	sm, err := sandbox.NewManager(ctx, sandboxCfg)
	if err != nil {
		telemetry.Error(ctx, "failed to initialize sandbox manager", otellog.String("error", err.Error()))
		return fail(err)
	}
	sandboxMgr = sm
	cleanups = append(cleanups, func() { _ = sandboxMgr.Close() })

	toolReg.Register(&sandbox.ExecTool{Manager: sandboxMgr})
	toolReg.Register(&sandbox.ReadTool{Manager: sandboxMgr})
	toolReg.Register(&sandbox.WriteTool{Manager: sandboxMgr})
	toolReg.Register(&kanban.SubmitTool{})
	toolReg.Register(&kanban.TaskContextTool{})

	skillStore := skill.NewSkillStore(ws, "skills")
	skillStore.SetBuiltinFS(skills.BuiltinFS())
	skillStore.SetGlobalConfig(cfg.Skills)
	if err := skillStore.BuildIndex(ctx); err != nil {
		telemetry.Warn(ctx, "skill: initial index build failed", otellog.String("error", err.Error()))
	}
	skillWatcher, swErr := skillStore.StartWatching(ctx)
	if swErr != nil {
		telemetry.Warn(ctx, "skill: fsnotify watcher disabled", otellog.String("error", swErr.Error()))
	}
	if skillWatcher != nil {
		cleanups = append(cleanups, func() { skillWatcher.Stop() })
	}

	skillExecutor := skill.NewSkillExecutor(skillStore, sandboxMgr)
	toolReg.Register(&skill.SkillTool{Store: skillStore, Executor: skillExecutor})
	extract.Register(toolReg, extract.New())

	pluginhost.InjectTools(pluginReg, toolReg)

	ltStore := memory.NewFileLongTermStore(ws, "", memory.WithMaxEntries(0))

	cpDir := filepath.Join(cfg.ConfigurePath, "checkpoints")
	checkpointStore, err := executor.NewFileCheckpointStore(executor.FileCheckpointConfig{
		Dir:            cpDir,
		MaxCheckpoints: 3,
	})
	if err != nil {
		telemetry.Warn(ctx, "checkpoint store disabled", otellog.String("error", err.Error()))
		checkpointStore = nil
	}

	memStore := memory.NewFileStore(ws, "memory")
	summaryStore := memory.NewFileSummaryStore(ws, "memory")
	memoryFactory := func(ctx context.Context, cfg model.MemoryConfig) (memory.Memory, error) {
		l, _ := llmResolver.Resolve(ctx, "")
		return memory.NewWithLLM(cfg, memStore, l, ltStore,
			memory.WithWorkspace(ws),
			memory.WithPrefix("memory"),
		)
	}

	memory.RegisterTools(toolReg, memory.ToolDeps{
		SummaryStore: summaryStore,
		MessageStore: memStore,
		Workspace:    ws,
		Prefix:       "memory",
		Config:       memory.DefaultDAGConfig(),
	})

	memExtractor := memory.NewMemoryExtractor(llmResolver, ltStore, memory.LongTermConfig{Enabled: true}, memory.ExtractorConfig{})

	graphExecutor := executor.NewLocalExecutor()
	platformCfg := &realm.PlatformDeps{
		Store:           appStore,
		Factory:         nodeFactory,
		Executor:        graphExecutor,
		Workspace:       ws,
		MemoryFactory:   memoryFactory,
		Extractor:       memExtractor,
		SummaryStore:    summaryStore,
		CheckpointStore: checkpointStore,
		StrategyResolver: func(a *model.Agent) workflow.Strategy {
			if gd := a.StrategyDef.AsGraph(); gd != nil {
				return adapter.FromDefinition(gd)
			}
			return nil
		},
	}
	runtimeMgr := realm.NewSingleRealmProvider(appStore, platformCfg, sandboxCfg, sandboxCfg.IdleTimeout)
	cleanups = append(cleanups, runtimeMgr.Close)

	telemetry.Info(ctx, "kanban: board restoration enabled, persisted cards will be loaded on runtime init")

	metatool.Register(toolReg, &metatool.Deps{
		Store:        appStore,
		Compiler:     graphCompiler,
		VersionStore: versionStore,
		SchemaReg:    schemaReg,
		ToolRegistry: toolReg,
		EventBus:     globalBus,
	})
	toolReg.Register(knowledge.NewSearchTool(knowledgeStore))
	toolReg.Register(knowledge.NewAddTool(knowledgeStore))

	ensureCoPilotAgent(ctx, appStore, knowledgeStore, templateReg)

	gw := initGateway(ctx, runtimeMgr, appStore)
	nr := gateway.NewNotificationRouter(gw, appStore)
	var schedOnce sync.Once
	runtimeMgr.OnRealmCreated(func(r *realm.Realm) {
		nr.SubscribeSession(ctx, r.Board())
		go persistKanbanCards(ctx, r.Board(), appStore)

		schedOnce.Do(func() {
			go initScheduler(ctx, runtimeMgr, appStore, globalBus)
		})
	})

	webFS, _ := fs.Sub(web.Dist, "dist")

	deps := api.ServerDeps{
		Store:      appStore,
		RuntimeMgr: runtimeMgr,
		Compiler:     graphCompiler,
		SchemaReg:    schemaReg,
		TemplateReg:  templateReg,
		PluginReg:    pluginReg,
		Knowledge:    knowledgeStore,
		VersionStore: versionStore,
		LTStore:      ltStore,
		EventBus:     globalBus,
		LLMResolver:  llmResolver,
		SkillStore:   skillStore,
		ToolRegistry: toolReg,
		PluginDir:    pluginDir,
		Monitoring: api.MonitoringConfig{
			ErrorRateWarn:        cfg.Monitoring.ErrorRateWarn,
			ErrorRateDown:        cfg.Monitoring.ErrorRateDown,
			LatencyP95WarnMs:     cfg.Monitoring.LatencyP95WarnMs,
			ConsecutiveBuckets:   cfg.Monitoring.ConsecutiveBuckets,
			NoSuccessDownMinutes: cfg.Monitoring.NoSuccessDownMinutes,
		},
	}
	deps.Gateway = gw

	if cfg.Auth.APIKey == "" {
		telemetry.Warn(ctx, "security: API key is not configured, authentication is disabled — set FLOWCRAFT_AUTH_API_KEY for production use")
	}

	server := api.NewServer(api.ServerConfig{
		Host:           cfg.Server.Host,
		Port:           cfg.Server.Port,
		APIKey:         cfg.Auth.APIKey,
		RateLimitRPS:   cfg.Server.RateLimitRPS,
		RateLimitBurst: cfg.Server.RateLimitBurst,
		MaxUploadSize:  cfg.Plugin.MaxUploadSize,
		WebFS:          webFS,
	}, deps)

	return server, runCleanups, nil
}

// persistKanbanCards watches card changes on a TaskBoard and persists
// each change to the store. Runs until the board's context is cancelled.
func persistKanbanCards(ctx context.Context, sb *kanban.TaskBoard, store model.Store) {
	if sb == nil || store == nil {
		return
	}
	ch := sb.Watch(sb.Context())
	for card := range ch {
		if card.Type == "result" {
			continue
		}
		query, targetAgentID, output := kanban.ExtractPayloadFieldsPublic(card.Payload)
		runID := ""
		if m, ok := card.Payload.(map[string]any); ok {
			runID, _ = m["run_id"].(string)
		}
		var metaMap map[string]any
		if card.Meta != nil {
			metaMap = make(map[string]any, len(card.Meta))
			for k, v := range card.Meta {
				metaMap[k] = v
			}
		}
		if err := store.SaveKanbanCard(ctx, &model.KanbanCard{
			KanbanCardModel: kanban.KanbanCardModel{
				ID:            card.ID,
				RuntimeID:     sb.ScopeID(),
				Type:          card.Type,
				Status:        string(card.Status),
				Producer:      card.Producer,
				Consumer:      card.Consumer,
				TargetAgentID: targetAgentID,
				Query:         query,
				Output:        output,
				Error:         card.Error,
				RunID:         runID,
				ElapsedMs:     card.UpdatedAt.Sub(card.CreatedAt).Milliseconds(),
				CreatedAt:     card.CreatedAt,
				UpdatedAt:     card.UpdatedAt,
			},
			Meta: metaMap,
		}); err != nil {
			telemetry.Warn(ctx, "kanban: persist card failed",
				otellog.String("runtime_id", sb.ScopeID()),
				otellog.String("card_id", card.ID),
				otellog.String("error", err.Error()))
		}
	}
}

// buildSandboxMounts constructs mount configurations for Docker sandboxes.
// Three scenarios: explicit HostDataDir (deprecated), DinD (Named Volume), bare-metal (bind mount).
func buildSandboxMounts(cfg *config.Config, workspaceRoot string) []sandbox.MountConfig {
	//nolint:staticcheck // SA1019 HostDataDir kept for backward-compatible Docker mounts.
	hostDataDir := cfg.Sandbox.HostDataDir
	if hostDataDir != "" {
		telemetry.Warn(context.Background(),
			"sandbox: HostDataDir is deprecated; consider switching to FLOWCRAFT_SANDBOX_DRIVER=local with Bubblewrap isolation",
			otellog.String("host_data_dir", hostDataDir))
		return []sandbox.MountConfig{
			{Source: filepath.Join(hostDataDir, "skills"), Target: "/workspace/skills", ReadOnly: true, Overlay: true},
			{Source: filepath.Join(hostDataDir, "data"), Target: "/workspace/data"},
		}
	}
	if isRunningInContainer() {
		volumeName := os.Getenv("FLOWCRAFT_SANDBOX_VOLUME_NAME")
		if volumeName == "" {
			volumeName = "flowcraft-workspace"
		}
		return []sandbox.MountConfig{
			{Type: "volume", Source: volumeName, Target: "/workspace"},
		}
	}
	return []sandbox.MountConfig{
		{Source: filepath.Join(workspaceRoot, "skills"), Target: "/workspace/skills", ReadOnly: true, Overlay: true},
		{Source: filepath.Join(workspaceRoot, "data"), Target: "/workspace/data"},
	}
}

func isRunningInContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// initScheduler starts the Kanban scheduler after the runtime is available.
// It is called in a goroutine (via sync.Once) from OnRealmCreated to
// avoid holding SingleRealmProvider's write lock during Resolve/Get.
func initScheduler(ctx context.Context, runtimeMgr *realm.SingleRealmProvider, store model.Store, bus event.EventBus) {
	rt, err := runtimeMgr.Get(ctx)
	if err != nil {
		telemetry.Error(ctx, "scheduler: failed to get runtime", otellog.String("error", err.Error()))
		return
	}
	k := rt.Kanban()
	if k == nil {
		telemetry.Warn(ctx, "scheduler: kanban not available, scheduler disabled")
		return
	}
	sched := k.Scheduler()
	if sched == nil {
		telemetry.Warn(ctx, "scheduler: no scheduler instance on kanban, scheduler disabled")
		return
	}

	loadAllSchedules(ctx, sched, store)

	n := sched.LoadFromBoard()
	if n > 0 {
		telemetry.Info(ctx, "scheduler: restored dynamic cron rules from board",
			otellog.Int("count", n))
	}

	sched.Start()
	telemetry.Info(ctx, "scheduler: started")

	subscribeScheduleChanges(ctx, sched, store, bus)
}

// loadAllSchedules reads all agent configs and syncs their schedule rules.
func loadAllSchedules(ctx context.Context, sched *kanban.Scheduler, store model.Store) {
	agents, _, err := store.ListAgents(ctx, model.ListOptions{Limit: model.MaxPageLimit})
	if err != nil {
		telemetry.Warn(ctx, "scheduler: failed to list agents", otellog.String("error", err.Error()))
		return
	}
	count := 0
	for _, a := range agents {
		jobs := toCronJobs(a.Config.Schedules)
		if len(jobs) > 0 {
			sched.SyncAgent(a.AgentID, jobs)
			count += len(jobs)
		}
	}
	telemetry.Info(ctx, "scheduler: loaded schedules",
		otellog.Int("agent_count", len(agents)),
		otellog.Int("schedule_count", count))
}

// subscribeScheduleChanges watches for EventAgentConfigChanged and reloads
// the affected agent's schedule rules.
func subscribeScheduleChanges(ctx context.Context, sched *kanban.Scheduler, store model.Store, bus event.EventBus) {
	sub, err := bus.Subscribe(ctx, event.EventFilter{
		Types: []event.EventType{event.EventAgentConfigChanged},
	})
	if err != nil {
		telemetry.Error(ctx, "scheduler: failed to subscribe to config events", otellog.String("error", err.Error()))
		return
	}
	go func() {
		defer func() { _ = sub.Close() }()
		for ev := range sub.Events() {
			agentID := ev.ActorID
			if agentID == "" {
				continue
			}
			agent, err := store.GetAgent(ctx, agentID)
			if err != nil {
				sched.RemoveAgent(agentID)
				telemetry.Info(ctx, "scheduler: agent removed",
					otellog.String("agent_id", agentID))
				continue
			}
			jobs := toCronJobs(agent.Config.Schedules)
			sched.SyncAgent(agentID, jobs)
			telemetry.Info(ctx, "scheduler: agent schedule synced",
				otellog.String("agent_id", agentID),
				otellog.Int("schedule_count", len(jobs)))
		}
	}()
}

// toCronJobs converts model.Schedule slice to kanban.CronJob slice.
func toCronJobs(schedules []model.Schedule) []kanban.CronJob {
	if len(schedules) == 0 {
		return nil
	}
	jobs := make([]kanban.CronJob, 0, len(schedules))
	for _, s := range schedules {
		jobs = append(jobs, kanban.CronJob{
			ID:       s.ID,
			Cron:     s.Cron,
			Query:    s.Query,
			Enabled:  s.Enabled,
			Timezone: s.Timezone,
			Source:   s.Source,
		})
	}
	return jobs
}
