package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/component"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/memory/retrieval/fusion"
	"github.com/GizClaw/flowcraft/memory/retrieval/hydrate"
	"github.com/GizClaw/flowcraft/memory/retrieval/pack"
	"github.com/GizClaw/flowcraft/memory/sources"
	"github.com/GizClaw/flowcraft/memory/storage"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	observationview "github.com/GizClaw/flowcraft/memory/views/observation"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

type failLifecycleWorkspace struct {
	workspace.Workspace
	mu        sync.Mutex
	match     string
	remaining int
}

func (ws *failLifecycleWorkspace) Rename(ctx context.Context, source, target string) error {
	ws.mu.Lock()
	if ws.remaining > 0 && strings.Contains(target, ws.match) {
		ws.remaining--
		ws.mu.Unlock()
		return errors.New("injected lifecycle publish failure")
	}
	ws.mu.Unlock()
	return ws.Workspace.Rename(ctx, source, target)
}

type lifecycleSearcher func(context.Context, component.SearchRequest) ([]component.Candidate, error)

func (searcher lifecycleSearcher) Search(ctx context.Context, request component.SearchRequest) ([]component.Candidate, error) {
	return searcher(ctx, request)
}

func TestFactOutboxObservationRecallReinforceMainline(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 6, 0, 0, 0, time.UTC)
	clock := &fixedClock{value: now}
	ws := workspace.NewMemWorkspace()
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	outbox, _ := NewWorkspaceOutbox(ws, clock)
	sink := &OutboxSink{Outbox: outbox, PolicyDigest: "policy", Branch: "integrate"}
	facts := newFactStore(t, ws, factview.WithClock(clock.Now), factview.WithPublicationSink(sink))
	observations := newObservationStore(t, ws, observationview.WithClock(clock.Now))
	events := newEventStore(t, ws)
	decay, _ := NewDecay(DecayConfig{HalfLife: time.Hour, RecencyWeight: 1}, clock)
	service, _ := NewService(ServiceConfig{Facts: facts, Observations: observations, Events: events, Decay: decay,
		Forget: ForgetConfig{Mode: ModeAuditOnly}})
	runner, _ := NewDreamingRunner(DreamingRunnerConfig{
		Outbox: outbox, Service: service, Scopes: []sdkmemory.Scope{scope}, Owner: "runner",
		LeaseTTL: time.Minute, Interval: time.Hour, PolicyDigest: "policy", Branch: "integrate",
	})
	source := sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "message"}
	if _, err := facts.Add(ctx, factview.AddRequest{
		ID: "fact", Scope: scope, ConversationID: "conversation", Predicate: "city", Entities: []string{"alice"},
		Content:    sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: "Alice lives in Paris"}}},
		Provenance: []sdkmemory.SourceRef{source},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	current, ok, err := observations.Current(ctx, scope, "entity-predicate:alice:city")
	if err != nil || !ok || current.FactID != "fact" {
		t.Fatalf("observation = %#v, %v, %v", current, ok, err)
	}
	search := lifecycleSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{{
			ID: "fact", Lane: "vector", Name: "fact", Score: 1, Source: source,
			Address: component.CandidateAddress{Kind: sdkmemory.ContextFact, ConversationID: "conversation", ItemID: "fact"},
		}}, nil
	})
	fusor, _ := fusion.New([]fusion.Lane{{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.Identity{}}})
	provider, _ := retrieval.NewProviderWithConfig(retrieval.ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Facts: facts}, Packer: pack.New(nil),
		RecallEvents: events, Visibility: events, Clock: clock.Now,
	})
	result, err := provider.Context(ctx, sdkmemory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "Paris",
		Budget: sdkmemory.Budget{MaxItems: 1, MaxTokens: 100}, RecallEventID: "invocation",
	})
	if err != nil || len(result.Items) != 1 {
		t.Fatalf("recall = %#v, %v", result, err)
	}
	access, ok, err := events.Access(ctx, scope, factContextIdentity(scope, "conversation", "fact"))
	if err != nil || !ok || access.AccessCount != 1 {
		t.Fatalf("reinforce = %#v, %v, %v", access, ok, err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("completed task replayed: %v", err)
	}
}

func TestReconcileRecoversFactAfterInitialOutboxWriteFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 6, 0, 0, 0, time.UTC)
	clock := &fixedClock{value: now}
	ws := &failLifecycleWorkspace{
		Workspace: workspace.NewMemWorkspace(),
		match:     "outbox/memory-lifecycle/v1/tasks/", remaining: 1,
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	outbox, _ := NewWorkspaceOutbox(ws, clock)
	sink := &OutboxSink{Outbox: outbox, PolicyDigest: "policy", Branch: "integrate"}
	facts := newFactStore(t, ws, factview.WithClock(clock.Now), factview.WithPublicationSink(sink))
	if _, err := facts.Add(ctx, factview.AddRequest{
		ID: "fact", Scope: scope, ConversationID: "conversation",
		Content:    sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: "durable before outbox"}}},
		Provenance: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "message"}},
	}); err == nil {
		t.Fatal("injected initial outbox write failure succeeded")
	}
	stored, ok, err := facts.Get(ctx, scope, "conversation", "fact")
	if err != nil || !ok || stored.Text != "durable before outbox" {
		t.Fatalf("fact was not durable: %#v, %v, %v", stored, ok, err)
	}
	events := newEventStore(t, ws)
	observations := newObservationStore(t, ws, observationview.WithClock(clock.Now))
	decay, _ := NewDecay(DecayConfig{HalfLife: time.Hour, RecencyWeight: 1}, clock)
	service, _ := NewService(ServiceConfig{
		Facts: facts, Observations: observations, Events: events, Decay: decay,
		Forget: ForgetConfig{Mode: ModeAuditOnly},
	})
	if err := service.ReconcileScope(ctx, outbox, scope, "policy", "integrate"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := outbox.LeaseNext(ctx, scope, "runner", time.Minute); err != nil || !found {
		t.Fatalf("reconciled task found=%v err=%v", found, err)
	}
}

func TestCompleteFailureReplayDoesNotDuplicateObservationSideEffect(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 6, 0, 0, 0, time.UTC)
	clock := &fixedClock{value: now}
	ws := &failLifecycleWorkspace{Workspace: workspace.NewMemWorkspace()}
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	outbox, _ := NewWorkspaceOutbox(ws, clock)
	sink := &OutboxSink{Outbox: outbox, PolicyDigest: "policy", Branch: "integrate"}
	facts := newFactStore(t, ws, factview.WithClock(clock.Now), factview.WithPublicationSink(sink))
	observations := newObservationStore(t, ws, observationview.WithClock(clock.Now))
	events := newEventStore(t, ws)
	decay, _ := NewDecay(DecayConfig{HalfLife: time.Hour, RecencyWeight: 1}, clock)
	service, _ := NewService(ServiceConfig{
		Facts: facts, Observations: observations, Events: events, Decay: decay,
		Forget: ForgetConfig{Mode: ModeAuditOnly},
	})
	runner, _ := NewDreamingRunner(DreamingRunnerConfig{
		Outbox: outbox, Service: service, Scopes: []sdkmemory.Scope{scope}, Owner: "runner",
		LeaseTTL: time.Minute, Interval: time.Hour, PolicyDigest: "policy", Branch: "integrate",
	})
	if _, err := facts.Add(ctx, factview.AddRequest{
		ID: "fact", Scope: scope, ConversationID: "conversation",
		Content:    sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: "exactly once observation"}}},
		Provenance: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "message"}},
	}); err != nil {
		t.Fatal(err)
	}
	ws.mu.Lock()
	ws.match, ws.remaining = "-completed-", 1
	ws.mu.Unlock()
	if err := runner.RunOnce(ctx); err == nil {
		t.Fatal("injected task completion failure succeeded")
	}
	clock.value = now.Add(2 * time.Minute)
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	values, err := observations.List(ctx, scope)
	observationEvents, eventsErr := observations.Events(ctx, scope)
	if err != nil || eventsErr != nil || len(values) != 1 || len(observationEvents) != 1 {
		t.Fatalf("replay duplicated side effects: values=%d events=%d err=%v/%v", len(values), len(observationEvents), err, eventsErr)
	}
}

func TestSoftVisibilityFiltersRealProviderResults(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 6, 0, 0, 0, time.UTC)
	clock := &fixedClock{value: now}
	ws := workspace.NewMemWorkspace()
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	outbox, _ := NewWorkspaceOutbox(ws, clock)
	sink := &OutboxSink{Outbox: outbox, PolicyDigest: "policy", Branch: "integrate"}
	facts := newFactStore(t, ws, factview.WithClock(clock.Now), factview.WithPublicationSink(sink))
	observations := newObservationStore(t, ws, observationview.WithClock(clock.Now))
	events := newEventStore(t, ws)
	decay, _ := NewDecay(DecayConfig{HalfLife: time.Hour, RecencyWeight: 1}, clock)
	service, _ := NewService(ServiceConfig{
		Facts: facts, Observations: observations, Events: events, Decay: decay,
		Forget: ForgetConfig{Mode: ModeSoftVisibility, EnableSoftVisibility: true, SoftForgetThreshold: .2},
	})
	runner, _ := NewDreamingRunner(DreamingRunnerConfig{
		Outbox: outbox, Service: service, Scopes: []sdkmemory.Scope{scope}, Owner: "runner",
		LeaseTTL: time.Minute, Interval: time.Hour, PolicyDigest: "policy", Branch: "integrate",
	})
	source := sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "message"}
	if _, err := facts.Add(ctx, factview.AddRequest{
		ID: "fact", Scope: scope, ConversationID: "conversation", EventTime: now.Add(-10 * time.Hour),
		Content:    sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: "old hidden memory"}}},
		Provenance: []sdkmemory.SourceRef{source},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	search := lifecycleSearcher(func(context.Context, component.SearchRequest) ([]component.Candidate, error) {
		return []component.Candidate{{
			ID: "fact", Lane: "vector", Name: "fact", Score: 1, Source: source,
			Address: component.CandidateAddress{Kind: sdkmemory.ContextFact, ConversationID: "conversation", ItemID: "fact"},
		}}, nil
	})
	fusor, _ := fusion.New([]fusion.Lane{{Name: "vector", Searcher: search, Weight: 1, Calibrator: fusion.Identity{}}})
	provider, _ := retrieval.NewProviderWithConfig(retrieval.ProviderConfig{
		Fusion: fusor, Hydrator: &hydrate.Composite{Facts: facts}, Packer: pack.New(nil), Visibility: events,
	})
	result, err := provider.Context(ctx, sdkmemory.ContextRequest{
		Scope: scope, ConversationID: "conversation", Query: "hidden",
		Budget: sdkmemory.Budget{MaxItems: 1, MaxTokens: 100},
	})
	if err != nil || len(result.Items) != 0 {
		t.Fatalf("soft-forgotten result remained visible: %#v err=%v", result, err)
	}
}

func TestDreamingRunnerDiscoversCatalogScopeAfterConstruction(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 6, 0, 0, 0, time.UTC)
	clock := &fixedClock{value: now}
	ws := workspace.NewMemWorkspace()
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "dynamic"}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	catalog, _ := sources.NewScopeCatalog(kvStore)
	outbox, _ := NewWorkspaceOutbox(ws, clock)
	sink := &OutboxSink{Outbox: outbox, PolicyDigest: "policy", Branch: "integrate"}
	facts := newFactStore(t, ws, factview.WithClock(clock.Now), factview.WithPublicationSink(sink))
	observations := newObservationStore(t, ws, observationview.WithClock(clock.Now))
	events := newEventStore(t, ws)
	decay, _ := NewDecay(DecayConfig{HalfLife: time.Hour, RecencyWeight: 1}, clock)
	service, _ := NewService(ServiceConfig{
		Facts: facts, Observations: observations, Events: events, Decay: decay,
		Forget: ForgetConfig{Mode: ModeAuditOnly},
	})
	runner, err := NewDreamingRunner(DreamingRunnerConfig{
		Outbox: outbox, Service: service, Catalog: catalog, Owner: "runner",
		LeaseTTL: time.Minute, Interval: time.Hour, PolicyDigest: "policy", Branch: "integrate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if _, err := facts.Add(ctx, factview.AddRequest{
		ID: "fact", Scope: scope, ConversationID: "conversation",
		Content:    sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: "dynamic scope"}}},
		Provenance: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceMessage, ID: "message"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	values, err := observations.List(ctx, scope)
	if err != nil || len(values) != 1 {
		t.Fatalf("dynamic observations=%#v err=%v", values, err)
	}
}

type concurrencyPhase struct {
	mu                sync.Mutex
	active            map[string]int
	maxByScope        map[string]int
	global, maxGlobal int
}

type retryPhase struct {
	mu        sync.Mutex
	calls     int
	failFirst bool
	delay     time.Duration
}

func (phase *retryPhase) RunLifecycleTask(ctx context.Context, _ Task) error {
	phase.mu.Lock()
	phase.calls++
	call := phase.calls
	phase.mu.Unlock()
	if phase.delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(phase.delay):
		}
	}
	if phase.failFirst && call == 1 {
		return context.DeadlineExceeded
	}
	return nil
}

func (phase *retryPhase) count() int {
	phase.mu.Lock()
	defer phase.mu.Unlock()
	return phase.calls
}

func TestDreamingRunnerHeartbeatsSlowTaskAcrossRunners(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	outbox, _ := NewWorkspaceOutbox(ws, nil)
	events := newEventStore(t, ws)
	decay, _ := NewDecay(DecayConfig{HalfLife: time.Hour, RecencyWeight: 1}, nil)
	phase := &retryPhase{delay: 100 * time.Millisecond}
	service, _ := NewService(ServiceConfig{
		Facts: &fakeFacts{}, Observations: &fakeObservations{}, Events: events, Decay: decay,
		Forget: ForgetConfig{Mode: ModeAuditOnly}, Compact: phase,
	})
	if _, err := outbox.Enqueue(ctx, Task{
		Scope: scope, FactID: "fact", StateEvent: EventFactPublished,
		PublicationID: "publication", RevisionDigest: "revision", PolicyDigest: "policy", Branch: "integrate",
	}); err != nil {
		t.Fatal(err)
	}
	config := DreamingRunnerConfig{
		Outbox: outbox, Service: service, Scopes: []sdkmemory.Scope{scope}, Owner: "runner",
		LeaseTTL: 30 * time.Millisecond, Interval: time.Hour, PolicyDigest: "policy", Branch: "integrate",
	}
	first, _ := NewDreamingRunner(config)
	config.Owner = "runner-two"
	second, _ := NewDreamingRunner(config)
	errs := make(chan error, 2)
	go func() { errs <- first.RunOnce(ctx) }()
	time.Sleep(50 * time.Millisecond)
	go func() { errs <- second.RunOnce(ctx) }()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if phase.count() != 1 {
		t.Fatalf("slow leased task executed %d times", phase.count())
	}
}

func TestDreamingRunnerRetriesNotificationFailureAndRecordsLastError(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	outbox, _ := NewWorkspaceOutbox(ws, nil)
	events := newEventStore(t, ws)
	decay, _ := NewDecay(DecayConfig{HalfLife: time.Hour, RecencyWeight: 1}, nil)
	phase := &retryPhase{failFirst: true}
	service, _ := NewService(ServiceConfig{
		Facts: &fakeFacts{}, Observations: &fakeObservations{}, Events: events, Decay: decay,
		Forget: ForgetConfig{Mode: ModeAuditOnly}, Compact: phase,
	})
	runner, _ := NewDreamingRunner(DreamingRunnerConfig{
		Outbox: outbox, Service: service, Scopes: []sdkmemory.Scope{scope}, Owner: "runner",
		LeaseTTL: time.Minute, Interval: time.Hour, PolicyDigest: "policy", Branch: "integrate",
	})
	if _, err := outbox.Enqueue(ctx, Task{
		Scope: scope, FactID: "fact", StateEvent: EventFactPublished,
		PublicationID: "publication", RevisionDigest: "revision", PolicyDigest: "policy", Branch: "integrate",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runner.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runner.Close() }()
	deadline := time.Now().Add(time.Second)
	for (phase.count() < 2 || runner.LastError() != nil) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if phase.count() != 2 {
		t.Fatalf("notification failure calls=%d last_error=%v", phase.count(), runner.LastError())
	}
	if err := runner.LastError(); err != nil {
		t.Fatalf("successful retry retained last error: %v", err)
	}
}

func (phase *concurrencyPhase) RunScope(_ context.Context, scope sdkmemory.Scope) error {
	key := scope.HardPartitionKey()
	phase.mu.Lock()
	phase.active[key]++
	phase.global++
	if phase.active[key] > phase.maxByScope[key] {
		phase.maxByScope[key] = phase.active[key]
	}
	if phase.global > phase.maxGlobal {
		phase.maxGlobal = phase.global
	}
	phase.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	phase.mu.Lock()
	phase.active[key]--
	phase.global--
	phase.mu.Unlock()
	return nil
}

func (phase *concurrencyPhase) RunLifecycleTask(ctx context.Context, task Task) error {
	return phase.RunScope(ctx, task.Scope)
}

func TestDreamingRunnerSerializesScopeAndIsolatesScopes(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	outbox, _ := NewWorkspaceOutbox(ws, nil)
	events := newEventStore(t, ws)
	clock := fixedClock{time.Date(2026, 8, 5, 6, 0, 0, 0, time.UTC)}
	decay, _ := NewDecay(DecayConfig{HalfLife: time.Hour, RecencyWeight: 1}, clock)
	facts := &fakeFacts{}
	observations := &fakeObservations{}
	phase := &concurrencyPhase{active: map[string]int{}, maxByScope: map[string]int{}}
	service, _ := NewService(ServiceConfig{Facts: facts, Observations: observations, Events: events, Decay: decay,
		Forget: ForgetConfig{Mode: ModeAuditOnly}, Compact: phase})
	scopeA := sdkmemory.Scope{RuntimeID: "a"}
	scopeB := sdkmemory.Scope{RuntimeID: "b"}
	for _, task := range []Task{
		{Scope: scopeA, FactID: "a1", StateEvent: EventFactPublished, PublicationID: "a1", RevisionDigest: "a1", PolicyDigest: "p", Branch: "integrate"},
		{Scope: scopeA, FactID: "a2", StateEvent: EventFactPublished, PublicationID: "a2", RevisionDigest: "a2", PolicyDigest: "p", Branch: "integrate"},
		{Scope: scopeB, FactID: "b1", StateEvent: EventFactPublished, PublicationID: "b1", RevisionDigest: "b1", PolicyDigest: "p", Branch: "integrate"},
	} {
		if _, err := outbox.Enqueue(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	runner, _ := NewDreamingRunner(DreamingRunnerConfig{
		Outbox: outbox, Service: service, Scopes: []sdkmemory.Scope{scopeA, scopeB},
		Owner: "runner", LeaseTTL: time.Minute, Interval: time.Hour, PolicyDigest: "p", Branch: "integrate",
	})
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if phase.maxByScope[scopeA.HardPartitionKey()] != 1 || phase.maxByScope[scopeB.HardPartitionKey()] != 1 ||
		phase.maxGlobal < 2 {
		t.Fatalf("concurrency per scope=%v global=%d", phase.maxByScope, phase.maxGlobal)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := runner.RunOnce(canceled); err == nil {
		t.Fatal("canceled dreaming run succeeded")
	}
}

type fakeFacts struct{}

func (*fakeFacts) Get(_ context.Context, scope sdkmemory.Scope, conversationID, id string) (factview.Fact, bool, error) {
	return factview.Fact{ID: id, Scope: scope, ConversationID: conversationID, CreatedAt: time.Now()}, true, nil
}
func (*fakeFacts) List(context.Context, sdkmemory.Scope, string, factview.ListOptions) ([]factview.Fact, error) {
	return nil, nil
}
func (*fakeFacts) ListScope(context.Context, sdkmemory.Scope) ([]factview.Fact, error) {
	return nil, nil
}
func (*fakeFacts) ListPublications(context.Context, sdkmemory.Scope) ([]factview.Publication, error) {
	return nil, nil
}

type fakeObservations struct {
	mu     sync.Mutex
	values []observationview.Observation
}

func (store *fakeObservations) Integrate(_ context.Context, fact factview.Fact, taskID string) (observationview.Observation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	value := observationview.Observation{ID: taskID, Scope: fact.Scope, FactID: fact.ID, State: observationview.StateActive}
	store.values = append(store.values, value)
	return value, nil
}
func (store *fakeObservations) List(_ context.Context, scope sdkmemory.Scope) ([]observationview.Observation, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	var result []observationview.Observation
	for _, value := range store.values {
		if value.Scope == scope {
			result = append(result, value)
		}
	}
	return result, nil
}
