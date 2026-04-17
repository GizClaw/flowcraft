package realm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/internal/model"
	"github.com/GizClaw/flowcraft/internal/sandbox"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph/executor"
	"github.com/GizClaw/flowcraft/sdk/kanban"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/sdk/workflow"

	otellog "go.opentelemetry.io/otel/log"
)

const defaultRealmID = "owner"

// VarDecisionKey is the board variable key for human-in-the-loop decision text.
const VarDecisionKey = "__decision"

// Realm is the execution domain: shared sandbox, Kanban board, and actor table.
type Realm struct {
	id string

	deps    *PlatformDeps
	runtime workflow.Runtime
	sandbox *sandbox.SandboxHandle
	kbn     *kanban.Kanban
	board   *kanban.Board
	bus     event.EventBus

	mu          sync.RWMutex
	actors      map[string]*AgentActor
	idleTimeout time.Duration
	done        chan struct{}
	once        sync.Once
}

// RealmStats summarizes the realm for debugging and monitoring.
type RealmStats struct {
	RealmID         string `json:"realm_id"`
	ActorCount      int    `json:"actor_count"`
	KanbanCardCount int    `json:"kanban_card_count"`
	SandboxLeases   int    `json:"sandbox_leases"`
}

// NewRealm constructs a Realm for the given realm ID.
func NewRealm(ctx context.Context, id string, store model.Store, deps *PlatformDeps, sbCfg sandbox.ManagerConfig, idleTimeout time.Duration) (*Realm, error) {
	if deps.Store == nil {
		deps.Store = store
	}

	handle, err := sandbox.NewSandboxHandle(ctx, id, sbCfg)
	if err != nil {
		return nil, err
	}

	rt := &Realm{
		id:          id,
		deps:        deps,
		runtime:     deps.BuildRuntime(),
		sandbox:     handle,
		actors:      make(map[string]*AgentActor),
		idleTimeout: idleTimeout,
		done:        make(chan struct{}),
	}
	if rt.idleTimeout <= 0 {
		rt.idleTimeout = 30 * time.Minute
	}
	rt.initKanban(ctx, store)
	rt.board = rt.kbn.Board()
	rt.bus = rt.kbn.Bus()
	go rt.reapIdleActors()
	return rt, nil
}

// ---------- accessors ----------

func (rt *Realm) ID() string                                { return rt.id }
func (rt *Realm) Bus() event.EventBus                       { return rt.bus }
func (rt *Realm) Board() *kanban.Board                      { return rt.board }
func (rt *Realm) SandboxHandle() *sandbox.SandboxHandle     { return rt.sandbox }
func (rt *Realm) Runtime() workflow.Runtime                 { return rt.runtime }
func (rt *Realm) Deps() *PlatformDeps                       { return rt.deps }
func (rt *Realm) CheckpointStore() executor.CheckpointStore { return rt.deps.CheckpointStore }

func (rt *Realm) Kanban() *kanban.Kanban {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.kbn
}

func (rt *Realm) isClosed() bool {
	select {
	case <-rt.done:
		return true
	default:
		return false
	}
}

// ---------- actor management ----------

// SendToAgent routes a request to the actor for the given agent.
func (rt *Realm) SendToAgent(_ context.Context, agent *model.Agent, req *workflow.Request, opts ...ActorOption) <-chan RunResult {
	if rt.isClosed() {
		done := make(chan RunResult, 1)
		done <- RunResult{Err: ErrActorStopped}
		return done
	}

	if bus := rt.Bus(); bus != nil {
		if req.Extensions == nil {
			req.Extensions = make(map[string]any)
		}
		req.Extensions["event_bus"] = bus
	}

	actor := rt.GetOrCreateActor(agent.AgentID, opts...)
	return actor.Send(agent, req)
}

// GetOrCreateActor returns the unique actor for the given agent within this realm.
func (rt *Realm) GetOrCreateActor(agentID string, opts ...ActorOption) *AgentActor {
	rt.mu.RLock()
	if actor, ok := rt.actors[agentID]; ok {
		rt.mu.RUnlock()
		return actor
	}
	rt.mu.RUnlock()

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if actor, ok := rt.actors[agentID]; ok {
		return actor
	}
	actorCtx := model.WithRuntimeID(context.Background(), rt.id)
	actorCtx = model.WithSandboxHandle(actorCtx, rt.sandbox)
	if rt.kbn != nil {
		actorCtx = kanban.WithKanban(actorCtx, rt.kbn)
	}
	if board := rt.Board(); board != nil {
		actorCtx = kanban.WithTaskBoard(actorCtx, board)
	}
	opts = append([]ActorOption{WithActorContext(actorCtx)}, opts...)
	bus := rt.Bus()
	actor := NewAgentActor(rt.id, agentID, bus, rt.executeAgent, opts...)
	rt.actors[agentID] = actor
	return actor
}

// AbortActor aborts the running actor, if any.
func (rt *Realm) AbortActor(agentID string) bool {
	rt.mu.RLock()
	actor := rt.actors[agentID]
	rt.mu.RUnlock()
	if actor == nil {
		return false
	}
	return actor.Abort()
}

// Actor returns the actor for a given agent ID, if present.
func (rt *Realm) Actor(agentID string) (*AgentActor, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	actor, ok := rt.actors[agentID]
	return actor, ok
}

// Stats returns a snapshot of the realm state.
func (rt *Realm) Stats() RealmStats {
	rt.mu.RLock()
	actorCount := len(rt.actors)
	rt.mu.RUnlock()

	cardCount := 0
	if board := rt.Board(); board != nil {
		cardCount = len(board.Cards())
	}
	sandboxLeases := 0
	if rt.sandbox != nil {
		sandboxLeases = rt.sandbox.UseCount()
	}

	return RealmStats{
		RealmID:         rt.id,
		ActorCount:      actorCount,
		KanbanCardCount: cardCount,
		SandboxLeases:   sandboxLeases,
	}
}

// Close releases realm-owned resources.
func (rt *Realm) Close() {
	rt.once.Do(func() {
		close(rt.done)
		rt.mu.Lock()
		defer rt.mu.Unlock()
		for _, actor := range rt.actors {
			actor.Stop()
		}
		rt.actors = map[string]*AgentActor{}
		if rt.kbn != nil {
			rt.kbn.Stop()
		}
		if rt.sandbox != nil {
			_ = rt.sandbox.Close()
		}
	})
}

// ======================== kanban init ========================

func (rt *Realm) initKanban(ctx context.Context, store model.Store) {
	board := rt.restoreOrNewBoard(ctx, store)
	exec := &kanbanAgentExecutor{realm: rt}

	sched := kanban.NewScheduler()

	var opts []kanban.Option
	if store != nil {
		opts = append(opts, kanban.WithAgentExecutor(exec))
		opts = append(opts, kanban.WithAgentValidator(rt.buildAgentValidator(store)))
	}
	opts = append(opts, kanban.WithScheduler(sched))
	k := kanban.New(ctx, board, opts...)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.kbn = k
	rt.board = k.Board()
	rt.bus = k.Bus()
}

func (rt *Realm) restoreOrNewBoard(ctx context.Context, store model.Store) *kanban.TaskBoard {
	if store != nil {
		cards, err := store.ListKanbanCards(ctx, rt.id)
		if err == nil && len(cards) > 0 {
			models := make([]*kanban.KanbanCardModel, len(cards))
			for i, c := range cards {
				m := c.KanbanCardModel
				models[i] = &m
			}
			telemetry.Info(ctx, "kanban: restored board from store",
				otellog.Int("card_count", len(cards)),
				otellog.String("runtime_id", rt.id))
			return kanban.RestoreTaskBoard(rt.id, models)
		}
	}
	return kanban.NewBoard(rt.id)
}

func (rt *Realm) buildAgentValidator(store model.Store) kanban.AgentValidator {
	return func(ctx context.Context, agentID string) error {
		if _, err := store.GetAgent(ctx, agentID); err == nil {
			return nil
		}
		agents, _, listErr := store.ListAgents(ctx, model.ListOptions{Limit: model.MaxPageLimit})
		if listErr != nil {
			return fmt.Errorf("agent %q not found", agentID)
		}
		ids := make([]string, 0, len(agents))
		for _, a := range agents {
			ids = append(ids, a.AgentID)
		}
		return fmt.Errorf("agent %q not found; available agent IDs: %v", agentID, ids)
	}
}

// ======================== idle reaper ========================

func (rt *Realm) reapIdleActors() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-rt.done:
			return
		case <-ticker.C:
			now := time.Now()
			rt.mu.Lock()
			for agentID, actor := range rt.actors {
				if actor.IsPersistent() || actor.IsRunning() {
					continue
				}
				if now.Sub(actor.LastActive()) > rt.idleTimeout {
					actor.Stop()
					delete(rt.actors, agentID)
				}
			}
			rt.mu.Unlock()
		}
	}
}

// ======================== SingleRealmProvider ========================

// SingleRealmProvider manages the default realm.
type SingleRealmProvider struct {
	store       model.Store
	deps        *PlatformDeps
	sbCfg       sandbox.ManagerConfig
	idleTimeout time.Duration
	onCreated   func(*Realm)

	mu    sync.RWMutex
	realm *Realm
}

// RealmProviderStats summarizes the default realm provider.
type RealmProviderStats struct {
	RealmCount int         `json:"realm_count"`
	ActorCount int         `json:"actor_count"`
	Current    *RealmStats `json:"current,omitempty"`
}

// NewSingleRealmProvider creates the default realm provider.
func NewSingleRealmProvider(store model.Store, deps *PlatformDeps, sbCfg sandbox.ManagerConfig, idleTimeout time.Duration) *SingleRealmProvider {
	return &SingleRealmProvider{
		store:       store,
		deps:        deps,
		sbCfg:       sbCfg,
		idleTimeout: idleTimeout,
	}
}

// OnRealmCreated registers a callback invoked when the realm is first created.
func (m *SingleRealmProvider) OnRealmCreated(fn func(*Realm)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onCreated = fn
}

// Resolve returns the owner realm, creating it if needed.
func (m *SingleRealmProvider) Resolve(ctx context.Context) (*Realm, error) {
	m.mu.RLock()
	if m.realm != nil {
		m.mu.RUnlock()
		return m.realm, nil
	}
	m.mu.RUnlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.realm != nil {
		return m.realm, nil
	}
	rt, err := NewRealm(ctx, defaultRealmID, m.store, m.deps, m.sbCfg, m.idleTimeout)
	if err != nil {
		return nil, err
	}
	m.realm = rt
	onCreated := m.onCreated
	if onCreated != nil {
		onCreated(rt)
	}
	return rt, nil
}

// Get is an alias for Resolve.
func (m *SingleRealmProvider) Get(ctx context.Context) (*Realm, error) {
	return m.Resolve(ctx)
}

// Current returns the existing realm without creating a new one.
func (m *SingleRealmProvider) Current() (*Realm, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.realm, m.realm != nil
}

// Stats returns a consistent snapshot of the realm provider.
func (m *SingleRealmProvider) Stats() RealmProviderStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.realm == nil {
		return RealmProviderStats{}
	}
	current := m.realm.Stats()
	return RealmProviderStats{
		RealmCount: 1,
		ActorCount: current.ActorCount,
		Current:    &current,
	}
}

// Close stops the default realm.
func (m *SingleRealmProvider) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.realm != nil {
		m.realm.Close()
		m.realm = nil
	}
}

var _ RealmProvider = (*SingleRealmProvider)(nil)
