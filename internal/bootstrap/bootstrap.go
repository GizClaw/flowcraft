// Package bootstrap wires all dependencies and returns a ready-to-serve
// *api.Server plus the assembled *platform.Platform.
package bootstrap

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/GizClaw/flowcraft/internal/api"
	auditcmd "github.com/GizClaw/flowcraft/internal/commands/audit"
	"github.com/GizClaw/flowcraft/internal/eventlog"
	"github.com/GizClaw/flowcraft/internal/gateway"
	"github.com/GizClaw/flowcraft/internal/metatool"
	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/platform"
	projection "github.com/GizClaw/flowcraft/internal/projection/common"
	"github.com/GizClaw/flowcraft/internal/realm"
	"github.com/GizClaw/flowcraft/internal/store"
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
	appStore, sqliteStore, storeCleanup, err := wireStore(ctx, cfg)
	if err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, storeCleanup)
	var _ *store.SQLiteStore = sqliteStore // use store package to satisfy import

	// --- eventlog + projection + retention ---
	var eventLog *eventlog.SQLiteLog
	eventLog, err = WireEventLog(ctx, sqliteStore)
	if err != nil {
		return fail(err)
	}
	projMgr := WireProjectionManager(sqliteStore)
	snapshots := projection.NewSQLiteSnapshots(sqliteStore.DB())
	r4, err := RegisterR4Projectors(projMgr, eventLog, snapshots)
	if err != nil {
		return fail(err)
	}
	auditCmds := auditcmd.New(eventLog)

	retentionStop := WireRetention(eventLog)
	cleanups = append(cleanups, retentionStop)

	if err := projMgr.Start(ctx, eventLog); err != nil {
		return fail(err)
	}
	cleanups = append(cleanups, func() { projMgr.Stop() })
	cleanups = append(cleanups, func() {
		if r4 != nil && r4.WebhookSender != nil {
			r4.WebhookSender.Stop()
		}
		if r4 != nil && r4.ChatAutoAck != nil {
			r4.ChatAutoAck.Stop()
		}
	})

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
	knowledgeStore, knowledgeWorker, knowledgeCleanup, err := wireKnowledge(ctx, ws, llmResolver, appStore)
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
		Store:             appStore,
		Realms:            runtimeMgr,
		Compiler:          graphCompiler,
		SchemaReg:         schemaReg,
		TemplateReg:       templateReg,
		PluginReg:         pluginReg,
		Knowledge:         knowledgeStore,
		KnowledgeWorker:   knowledgeWorker,
		VersionStore:      versionStore,
		LTStore:           ltStore,
		EventBus:          globalBus,
		LLMResolver:       llmResolver,
		SkillStore:        skillStore,
		ToolRegistry:      toolReg,
		ProjectionManager: projMgr,
	}

	// --- seed data ---
	ensureCoPilotAgent(ctx, appStore, knowledgeStore, knowledgeWorker, templateReg)
	recoverPendingKnowledgeDocs(ctx, appStore, knowledgeWorker)

	// --- JWT auth ---
	jwtCfg, err := wireAuth(ctx, appStore)
	if err != nil {
		return fail(err)
	}

	// --- gateway + HTTP ---
	gw := initGateway(ctx, runtimeMgr, appStore)
	nr := gateway.NewNotificationRouter(gw, appStore)

	pluginDir := cfg.Plugin.Dir
	if pluginDir == "" {
		pluginDir = filepath.Join(workspaceRoot, "plugins")
	}

	server := wireHTTP(cfg, plat, gw, jwtCfg, pluginDir, eventLog, auditCmds)

	// --- realm callbacks ---
	var schedOnce sync.Once
	runtimeMgr.OnRealmCreated(func(r *realm.Realm) {
		nr.SubscribeSession(ctx, r.Board())
		go persistKanbanCards(ctx, r.Board(), appStore)

		// §2.4 step 9: attach bridges BEFORE scheduler.Start() so the very
		// first cron tick is already observed by the eventlog. Both bridges
		// outlive this callback; they exit when the board's context is
		// cancelled (i.e., when the realm closes). Errors at this point are
		// fatal-ish for eventlog observability so we surface them to the
		// realm log; bootstrap itself stays alive because operators can
		// still drive the system via direct sdk calls.
		kb, cb, err := eventlog.BootKanbanWithBridge(r.Board().Context(), eventLog, r.Board())
		if err != nil {
			telemetry.Error(ctx, "kanban bridge: attach failed",
				otellog.String("realm", r.ID()),
				otellog.String("error", err.Error()))
			return
		}

		schedOnce.Do(func() {
			// §2.4 step 10: scheduler.Start() runs only after both bridges
			// are attached. initScheduler is responsible for that ordering
			// and for wiring lifecycle envelope publishing.
			go initScheduler(ctx, runtimeMgr, appStore, globalBus, cb)
		})

		// Stop bridges when the realm shuts down (otherwise the
		// goroutines leak past process shutdown and would race with
		// eventLog.Close on a real shutdown).
		go func() {
			<-r.Board().Context().Done()
			_ = kb.Close()
			_ = cb.Close()
		}()
	})

	return plat, server, runCleanups, nil
}

// persistKanbanCards watches card changes on a Board and persists
// each change to the store. Runs until the board's context is cancelled.
func persistKanbanCards(ctx context.Context, sb *kanban.Board, store model.Store) {
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

// initScheduler starts the Kanban scheduler after the runtime is available.
// It is called in a goroutine (via sync.Once) from OnRealmCreated to
// avoid holding SingleRealmProvider's write lock during Resolve/Get.
//
// §2.4 step 10: scheduler.Start() must run AFTER bridge_cron has been
// attached. Caller passes the cron bridge so we can publish lifecycle
// envelopes (cron.rule.created/changed/disabled) for every rule we sync.
func initScheduler(ctx context.Context, runtimeMgr *realm.SingleRealmProvider, store model.Store, bus event.EventBus, cb *eventlog.CronBridge) {
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

	runtimeID := rt.ID()
	loadAllSchedules(ctx, sched, store, cb, runtimeID)

	n := sched.LoadFromBoard()
	if n > 0 {
		telemetry.Info(ctx, "scheduler: restored dynamic cron rules from board",
			otellog.Int("count", n))
	}

	sched.Start()
	telemetry.Info(ctx, "scheduler: started")

	subscribeScheduleChanges(ctx, sched, store, bus, cb, runtimeID)
}

// loadAllSchedules reads all agent configs and syncs their schedule rules.
// Each rule that is loaded results in a cron.rule.created envelope so the
// eventlog has a complete history independent of the agent config table.
func loadAllSchedules(ctx context.Context, sched *kanban.Scheduler, store model.Store, cb *eventlog.CronBridge, runtimeID string) {
	agents, _, err := store.ListAgents(ctx, model.ListOptions{Limit: model.MaxPageLimit})
	if err != nil {
		telemetry.Warn(ctx, "scheduler: failed to list agents", otellog.String("error", err.Error()))
		return
	}
	count := 0
	for _, a := range agents {
		jobs := toCronJobs(a.Config.Schedules)
		if len(jobs) == 0 {
			continue
		}
		sched.SyncAgent(a.AgentID, jobs)
		count += len(jobs)
		publishRuleLifecycle(ctx, cb, runtimeID, a.AgentID, jobs)
	}
	telemetry.Info(ctx, "scheduler: loaded schedules",
		otellog.Int("agent_count", len(agents)),
		otellog.Int("schedule_count", count))
}

// publishRuleLifecycle emits cron.rule.created envelopes for each enabled
// rule, and cron.rule.disabled for each rule whose Enabled is explicitly
// false.
func publishRuleLifecycle(ctx context.Context, cb *eventlog.CronBridge, runtimeID, agentID string, jobs []kanban.CronJob) {
	if cb == nil {
		return
	}
	for _, j := range jobs {
		evt := eventlog.CronRuleEvent{
			RuleID:        j.ID,
			RuntimeID:     runtimeID,
			Expression:    j.Cron,
			Timezone:      j.Timezone,
			TargetAgentID: agentID,
			Query:         j.Query,
			Enabled:       j.Enabled == nil || *j.Enabled,
		}
		var err error
		if !evt.Enabled {
			evt.DisabledAt = eventlog.NowRFC3339Nano()
			err = cb.PublishRuleDisabled(ctx, evt)
		} else {
			err = cb.PublishRuleCreated(ctx, evt)
		}
		if err != nil {
			telemetry.Warn(ctx, "scheduler: publish rule lifecycle failed",
				otellog.String("rule_id", j.ID),
				otellog.String("error", err.Error()))
		}
	}
}

// subscribeScheduleChanges watches for EventAgentConfigChanged and reloads
// the affected agent's schedule rules. Each Sync emits cron.rule.changed
// envelopes; full agent removal emits cron.rule.disabled envelopes for
// each previously-known rule.
func subscribeScheduleChanges(ctx context.Context, sched *kanban.Scheduler, store model.Store, bus event.EventBus, cb *eventlog.CronBridge, runtimeID string) {
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
			publishRuleChanges(ctx, cb, runtimeID, agentID, jobs)
			telemetry.Info(ctx, "scheduler: agent schedule synced",
				otellog.String("agent_id", agentID),
				otellog.Int("schedule_count", len(jobs)))
		}
	}()
}

// publishRuleChanges emits cron.rule.changed for enabled rules and
// cron.rule.disabled for those whose Enabled field is explicitly false.
func publishRuleChanges(ctx context.Context, cb *eventlog.CronBridge, runtimeID, agentID string, jobs []kanban.CronJob) {
	if cb == nil {
		return
	}
	for _, j := range jobs {
		evt := eventlog.CronRuleEvent{
			RuleID:        j.ID,
			RuntimeID:     runtimeID,
			Expression:    j.Cron,
			Timezone:      j.Timezone,
			TargetAgentID: agentID,
			Query:         j.Query,
			Enabled:       j.Enabled == nil || *j.Enabled,
		}
		var err error
		if !evt.Enabled {
			evt.DisabledAt = eventlog.NowRFC3339Nano()
			err = cb.PublishRuleDisabled(ctx, evt)
		} else {
			err = cb.PublishRuleChanged(ctx, evt)
		}
		if err != nil {
			telemetry.Warn(ctx, "scheduler: publish rule change failed",
				otellog.String("rule_id", j.ID),
				otellog.String("error", err.Error()))
		}
	}
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
