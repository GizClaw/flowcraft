// Package bootstrap wires all dependencies and returns a ready-to-serve
// *api.Server plus the assembled *platform.Platform.
package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/GizClaw/flowcraft/internal/api"
	"github.com/GizClaw/flowcraft/internal/config"
	"github.com/GizClaw/flowcraft/internal/gateway"
	"github.com/GizClaw/flowcraft/internal/metatool"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/platform"
	"github.com/GizClaw/flowcraft/internal/realm"
	"github.com/GizClaw/flowcraft/internal/sandbox"
	"github.com/GizClaw/flowcraft/internal/template"
	"github.com/GizClaw/flowcraft/internal/version"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph/compiler"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
	_ "github.com/GizClaw/flowcraft/sdk/graph/node/scriptnode" // register built-in script node types
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdkx/knowledge"

	otellog "go.opentelemetry.io/otel/log"
)

// Run wires all dependencies and returns the assembled Platform, a
// ready-to-serve HTTP server, a cleanup function (called in reverse-init
// order), and any bootstrap error.
func Run(ctx context.Context) (*platform.Platform, *api.Server, func(), error) {
	var cleanups []func()
	runCleanups := func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}
	fail := func(err error) (*platform.Platform, *api.Server, func(), error) {
		runCleanups()
		return nil, nil, nil, err
	}

	// --- config + telemetry ---
	cfg, telemetryCleanup, err := wireConfig(ctx)
	if err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, telemetryCleanup)

	// --- store ---
	appStore, storeCleanup, err := wireStore(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, storeCleanup)

	// --- registries + resolvers ---
	toolReg := tool.NewRegistry()

	schemaReg := node.NewSchemaRegistry()
	node.RegisterBuiltinSchemas(schemaReg)
	templateReg := template.NewRegistry()
	templateReg.RegisterBuiltins()
	templateReg.SetStore(appStore)
	if err := templateReg.LoadFromStore(ctx); err != nil {
		telemetry.Warn(ctx, "template: load from store failed", otellog.String("error", err.Error()))
	}

	llmResolver := llm.DefaultResolver(appStore)

	// --- sandbox + workspace ---
	ws, sandboxMgr, sandboxCfg, sandboxCleanup, err := wireSandbox(ctx, cfg, toolReg)
	if err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, sandboxCleanup)

	workspaceRoot := sandboxCfg.RootDir

	// --- knowledge ---
	knowledgeStore, knowledgeCleanup, err := wireKnowledge(ctx, ws, llmResolver)
	if err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, knowledgeCleanup)

	// --- plugins + node factory ---
	pluginReg, nodeFactory, pluginCleanup, err := wirePlugin(ctx, cfg, ws, workspaceRoot, llmResolver, toolReg, schemaReg, sandboxMgr)
	if err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, pluginCleanup)

	// --- knowledge node ---
	knowledge.RegisterNode(knowledgeStore)
	schemaReg.Register(knowledge.KnowledgeNodeSchema())

	// --- skills ---
	skillStore, skillCleanup, err := wireSkill(ctx, ws, cfg, sandboxMgr, toolReg)
	if err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, skillCleanup)

	// --- version store + event bus ---
	versionStore := version.NewVersionStore(appStore)
	graphCompiler := compiler.NewCompiler()

	globalBus := event.NewMemoryBus()
	cleanups = append(cleanups, func() { _ = globalBus.Close() })

	// --- metatool + knowledge tools ---
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

	// --- realm ---
	runtimeMgr, ltStore, realmCleanup, err := wireRealm(ctx, appStore, cfg, ws, nodeFactory, llmResolver, toolReg, sandboxCfg)
	if err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, realmCleanup)

	telemetry.Info(ctx, "kanban: board restoration enabled, persisted cards will be loaded on runtime init")

	// --- assemble platform ---
	plat := &platform.Platform{
		Store:        appStore,
		Realms:       runtimeMgr,
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
	}

	// --- seed data ---
	ensureCoPilotAgent(ctx, appStore, knowledgeStore, templateReg)

	// --- gateway + HTTP ---
	gw := initGateway(ctx, runtimeMgr, appStore)
	nr := gateway.NewNotificationRouter(gw, appStore)

	pluginDir := cfg.Plugin.Dir
	if pluginDir == "" {
		pluginDir = filepath.Join(workspaceRoot, "plugins")
	}

	server := wireHTTP(cfg, plat, gw, pluginDir)

	if cfg.Auth.APIKey == "" {
		telemetry.Warn(ctx, "security: API key is not configured, authentication is disabled — set FLOWCRAFT_AUTH_API_KEY for production use")
	}

	// --- realm callbacks ---
	var schedOnce sync.Once
	runtimeMgr.OnRealmCreated(func(r *realm.Realm) {
		nr.SubscribeSession(ctx, r.Board())
		go persistKanbanCards(ctx, r.Board(), appStore)

		schedOnce.Do(func() {
			go initScheduler(ctx, runtimeMgr, appStore, globalBus)
		})
	})

	return plat, server, runCleanups, nil
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
