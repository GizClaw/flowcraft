package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/sources"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	observations "github.com/GizClaw/flowcraft/memory/views/observation"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// OutboxSink adapts fact publication to the durable lifecycle outbox.
type OutboxSink struct {
	Outbox       *WorkspaceOutbox
	PolicyDigest string
	Branch       string
}

func (sink *OutboxSink) PublishFact(ctx context.Context, publication factview.Publication) error {
	if sink == nil || sink.Outbox == nil {
		return errors.New("memory lifecycle: outbox sink is incomplete")
	}
	event := EventFactPublished
	if publication.Event == "merged" {
		event = EventFactMerged
	}
	_, err := sink.Outbox.Enqueue(ctx, Task{
		Scope: publication.Fact.Scope, ConversationID: publication.Fact.ConversationID,
		FactID: publication.Fact.ID, StateEvent: event, PublicationID: publication.PublicationID,
		RevisionDigest: publication.RevisionDigest, PolicyDigest: sink.PolicyDigest, Branch: sink.Branch,
	})
	return err
}

type FactReader interface {
	Get(context.Context, sdkmemory.Scope, string, string) (factview.Fact, bool, error)
	List(context.Context, sdkmemory.Scope, string, factview.ListOptions) ([]factview.Fact, error)
	ListScope(context.Context, sdkmemory.Scope) ([]factview.Fact, error)
	ListPublications(context.Context, sdkmemory.Scope) ([]factview.Publication, error)
}

type ObservationStore interface {
	Integrate(context.Context, factview.Fact, string) (observations.Observation, error)
	List(context.Context, sdkmemory.Scope) ([]observations.Observation, error)
}

type ScopePhase interface {
	RunScope(context.Context, sdkmemory.Scope) error
}
type ScopePhaseFunc func(context.Context, sdkmemory.Scope) error

func (function ScopePhaseFunc) RunScope(ctx context.Context, scope sdkmemory.Scope) error {
	return function(ctx, scope)
}

type TaskPhase interface {
	RunLifecycleTask(context.Context, Task) error
}
type TaskPhaseFunc func(context.Context, Task) error

func (function TaskPhaseFunc) RunLifecycleTask(ctx context.Context, task Task) error {
	return function(ctx, task)
}

// NoopTaskPhase is an explicit policy choice for a disabled optional phase.
type NoopTaskPhase struct{}

func (NoopTaskPhase) RunLifecycleTask(context.Context, Task) error { return nil }

type RepairPhase interface {
	RunRepair(context.Context, Task) (RepairPlan, error)
}

type RepairPhaseFunc func(context.Context, Task) (RepairPlan, error)

func (function RepairPhaseFunc) RunRepair(ctx context.Context, task Task) (RepairPlan, error) {
	return function(ctx, task)
}

type ServiceConfig struct {
	Facts        FactReader
	Observations ObservationStore
	Events       *EventStore
	Decay        *Decay
	Forget       ForgetConfig
	Compact      TaskPhase
	Repair       RepairPhase
	DAG          *DAG
	Checkpoints  CheckpointStore
	Effects      EffectSink
}

// Service executes the configured durable lifecycle DAG for one leased task.
type Service struct {
	config ServiceConfig
	dag    *DAG
}

func NewService(config ServiceConfig) (*Service, error) {
	if config.Facts == nil || config.Observations == nil || config.Events == nil || config.Decay == nil {
		return nil, errors.New("memory lifecycle: facts, observations, events, and decay are required")
	}
	if config.Forget.Mode == "" {
		config.Forget.Mode = ModeAuditOnly
	}
	if config.Forget.Mode != ModeAuditOnly && config.Forget.Mode != ModeSoftVisibility {
		return nil, errors.New("memory lifecycle: invalid forget mode")
	}
	if config.Forget.Mode == ModeSoftVisibility && !config.Forget.EnableSoftVisibility {
		return nil, errors.New("memory lifecycle: soft visibility mode requires explicit enable")
	}
	if config.DAG == nil {
		var err error
		config.DAG, err = Build(nil, Spec{})
		if err != nil {
			return nil, err
		}
	}
	if config.Checkpoints == nil {
		config.Checkpoints = newMemoryCheckpointStore()
	}
	if config.Compact == nil {
		config.Compact = NoopTaskPhase{}
	}
	return &Service{config: config, dag: config.DAG}, nil
}

func (service *Service) RunTask(ctx context.Context, task Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fact, found, err := service.config.Facts.Get(ctx, task.Scope, task.ConversationID, task.FactID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("memory lifecycle: fact %q not found", task.FactID)
	}
	state := &RunState{
		task: task,
		fact: cloneLifecycleFact(fact),
		Services: Services{
			Effects:   service.config.Effects,
			integrate: service.integrate,
			compact:   service.compact,
			decay:     service.decay,
			forget:    service.forget,
			repair:    service.repair,
		},
	}
	return service.dag.Run(ctx, state, service.config.Checkpoints)
}

func (service *Service) integrate(ctx context.Context, state *RunState) error {
	task := state.Task()
	value, err := service.config.Observations.Integrate(ctx, state.Fact(), task.ID)
	if err != nil {
		return err
	}
	cloned := cloneObservation(value)
	state.Observation = &cloned
	values, err := service.config.Observations.List(ctx, task.Scope)
	if err != nil {
		return err
	}
	state.Observations = cloneObservations(values)
	return nil
}

func (service *Service) compact(ctx context.Context, state *RunState) error {
	return service.config.Compact.RunLifecycleTask(ctx, state.Task())
}

func (service *Service) decay(ctx context.Context, state *RunState) error {
	task := state.Task()
	values := state.Observations
	if values == nil {
		var err error
		values, err = service.config.Observations.List(ctx, task.Scope)
		if err != nil {
			return err
		}
		state.Observations = cloneObservations(values)
	}
	snapshots := make([]ScoreSnapshot, 0, len(values))
	for _, value := range values {
		if err := ctx.Err(); err != nil {
			return err
		}
		identity := factContextIdentity(task.Scope, value.ConversationID, value.FactID)
		access, recalled, err := service.config.Events.Access(ctx, task.Scope, identity)
		if err != nil {
			return err
		}
		relevance := 0.0
		if recalled {
			relevance = access.LastRecallScore
		}
		snapshot := service.config.Decay.Score(DecayInput{
			BaseTime: value.EventTime, LastAccessAt: access.LastRecallAt,
			AccessCount: access.AccessCount, Relevance: relevance,
		})
		snapshot.ObservationID = value.ID
		snapshot.SourceRefs = append([]sdkmemory.SourceRef(nil), value.Provenance...)
		snapshots = append(snapshots, snapshot)
	}
	state.Scores = snapshots
	return nil
}

func (service *Service) forget(ctx context.Context, state *RunState) error {
	task := state.Task()
	plan := PlanForget(service.config.Forget, state.Scores, time.Now().UTC())
	state.ForgetPlan = &plan
	if service.config.Forget.Mode != ModeSoftVisibility || !service.config.Forget.EnableSoftVisibility {
		return nil
	}
	values := state.Observations
	if values == nil {
		var err error
		values, err = service.config.Observations.List(ctx, task.Scope)
		if err != nil {
			return err
		}
	}
	byID := make(map[string]observations.Observation, len(values))
	for _, value := range values {
		byID[value.ID] = value
	}
	for _, candidate := range plan.Candidates {
		if !candidate.Apply {
			continue
		}
		value, ok := byID[candidate.ObservationID]
		if !ok {
			continue
		}
		identity := factContextIdentity(task.Scope, value.ConversationID, value.FactID)
		eventID := digest("visibility", []byte(plan.ID+"\x00"+identity))
		if err := service.config.Events.SetSoftForgotten(ctx, VisibilityEvent{
			ID: eventID, Scope: task.Scope, ObservationID: identity,
			SoftForgotten: true, PlanID: plan.ID, Time: plan.CreatedAt,
		}); err != nil {
			return err
		}
	}
	return nil
}

func factContextIdentity(scope sdkmemory.Scope, conversationID, factID string) string {
	return (sdkmemory.ContextAddress{
		Kind: sdkmemory.ContextFact, ConversationID: conversationID, ItemID: factID,
	}).Identity(scope)
}

func (service *Service) repair(ctx context.Context, state *RunState) error {
	task := state.Task()
	if service.config.Repair == nil {
		plan, err := InspectRepairContext(ctx, task.Scope, RepairInput{})
		if err != nil {
			return err
		}
		state.RepairPlan = &plan
		return nil
	}
	plan, err := service.config.Repair.RunRepair(ctx, task)
	if err != nil {
		return err
	}
	state.RepairPlan = &plan
	return nil
}

func cloneLifecycleFact(value factview.Fact) factview.Fact {
	value.Content = value.Content.Clone()
	value.Entities = append([]string(nil), value.Entities...)
	value.LinkedMemoryIDs = append([]string(nil), value.LinkedMemoryIDs...)
	value.Provenance = append([]sdkmemory.SourceRef(nil), value.Provenance...)
	value.Metadata = value.Metadata.Clone()
	return value
}

func cloneObservation(value observations.Observation) observations.Observation {
	value.Provenance = append([]sdkmemory.SourceRef(nil), value.Provenance...)
	return value
}

func cloneObservations(values []observations.Observation) []observations.Observation {
	result := make([]observations.Observation, len(values))
	for index, value := range values {
		result[index] = cloneObservation(value)
	}
	return result
}

// ReconcileFacts recovers publication/outbox gaps by deterministically
// re-enqueuing every fact in a conversation.
func (service *Service) ReconcileFacts(ctx context.Context, outbox *WorkspaceOutbox, scope sdkmemory.Scope, conversationID, policyDigest, branch string) error {
	publications, err := service.config.Facts.ListPublications(ctx, scope)
	if err != nil {
		return err
	}
	for _, publication := range publications {
		if publication.Fact.ConversationID != conversationID {
			continue
		}
		if _, err := outbox.Enqueue(ctx, taskForPublication(publication, policyDigest, branch)); err != nil {
			return err
		}
	}
	return nil
}

func (service *Service) ReconcileScope(ctx context.Context, outbox *WorkspaceOutbox, scope sdkmemory.Scope, policyDigest, branch string) error {
	publications, err := service.config.Facts.ListPublications(ctx, scope)
	if err != nil {
		return err
	}
	for _, publication := range publications {
		if _, err := outbox.Enqueue(ctx, taskForPublication(publication, policyDigest, branch)); err != nil {
			return err
		}
	}
	return nil
}

func taskForPublication(publication factview.Publication, policyDigest, branch string) Task {
	event := EventFactPublished
	if publication.Event == "merged" {
		event = EventFactMerged
	}
	return Task{
		Scope: publication.Fact.Scope, ConversationID: publication.Fact.ConversationID,
		FactID: publication.Fact.ID, StateEvent: event, PublicationID: publication.PublicationID,
		RevisionDigest: publication.RevisionDigest, PolicyDigest: policyDigest, Branch: branch,
	}
}

type DreamingRunnerConfig struct {
	Outbox       *WorkspaceOutbox
	Service      *Service
	Catalog      *sources.ScopeCatalog
	Scopes       []sdkmemory.Scope
	Owner        string
	LeaseTTL     time.Duration
	Interval     time.Duration
	Periodic     bool
	PolicyDigest string
	Branch       string
}

type DreamingRunner struct {
	config  DreamingRunnerConfig
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	lastErr error
}

var dreamingScopeLocks sync.Map

func NewDreamingRunner(config DreamingRunnerConfig) (*DreamingRunner, error) {
	if config.Outbox == nil || config.Service == nil || strings.TrimSpace(config.Owner) == "" ||
		config.LeaseTTL <= 0 || (config.Periodic && config.Interval <= 0) ||
		strings.TrimSpace(config.PolicyDigest) == "" || strings.TrimSpace(config.Branch) == "" {
		return nil, errors.New("memory lifecycle dreaming runner: outbox, service, owner, lease ttl, and interval are required")
	}
	for _, scope := range config.Scopes {
		if err := scope.Validate(); err != nil {
			return nil, err
		}
	}
	return &DreamingRunner{config: config}, nil
}

// RunOnce processes scopes concurrently while each hard scope is serialized
// across all runners in this process.
func (runner *DreamingRunner) RunOnce(ctx context.Context) error {
	scopes := append([]sdkmemory.Scope(nil), runner.config.Scopes...)
	if runner.config.Catalog != nil {
		discovered, err := runner.config.Catalog.List(ctx)
		if err != nil {
			return err
		}
		scopes = append(scopes, discovered...)
	}
	unique := make(map[sdkmemory.Scope]struct{}, len(scopes))
	deduplicated := scopes[:0]
	for _, scope := range scopes {
		if _, exists := unique[scope]; exists {
			continue
		}
		unique[scope] = struct{}{}
		deduplicated = append(deduplicated, scope)
	}
	scopes = deduplicated
	sort.Slice(scopes, func(i, j int) bool { return scopes[i].HardPartitionKey() < scopes[j].HardPartitionKey() })
	var wait sync.WaitGroup
	errorsByScope := make(chan error, len(scopes))
	for _, scope := range scopes {
		scope := scope
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := runner.runScope(ctx, scope); err != nil {
				errorsByScope <- err
			}
		}()
	}
	wait.Wait()
	close(errorsByScope)
	var failures []error
	for err := range errorsByScope {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (runner *DreamingRunner) runScope(ctx context.Context, scope sdkmemory.Scope) error {
	value, _ := dreamingScopeLocks.LoadOrStore(scope.HardPartitionKey(), &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	if err := runner.config.Service.ReconcileScope(ctx, runner.config.Outbox, scope,
		runner.config.PolicyDigest, runner.config.Branch); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		lease, found, err := runner.config.Outbox.LeaseNext(ctx, scope, runner.config.Owner, runner.config.LeaseTTL)
		if err != nil || !found {
			return err
		}
		if err := runner.runLeasedTask(ctx, lease); err != nil {
			_ = runner.config.Outbox.Fail(context.WithoutCancel(ctx), lease.Task.ID, lease.Token, err)
			return err
		}
		if err := runner.config.Outbox.Complete(ctx, lease.Task.ID, lease.Token); err != nil {
			return err
		}
	}
}

func (runner *DreamingRunner) runLeasedTask(ctx context.Context, lease Lease) error {
	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runner.config.Service.RunTask(taskCtx, lease.Task) }()
	heartbeat := runner.config.LeaseTTL / 3
	if heartbeat <= 0 {
		heartbeat = time.Millisecond
	}
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case err := <-result:
			return err
		case <-ctx.Done():
			cancel()
			<-result
			return ctx.Err()
		case <-ticker.C:
			if _, err := runner.config.Outbox.Renew(ctx, lease.Task.ID, lease.Token, runner.config.LeaseTTL); err != nil {
				cancel()
				<-result
				return fmt.Errorf("memory lifecycle dreaming runner: lease lost: %w", err)
			}
		}
	}
}

func (runner *DreamingRunner) Start(ctx context.Context) error {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.cancel != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	runner.cancel, runner.done = cancel, make(chan struct{})
	go func() {
		defer close(runner.done)
		var ticks <-chan time.Time
		var ticker *time.Ticker
		if runner.config.Periodic {
			ticker = time.NewTicker(runner.config.Interval)
			ticks = ticker.C
			defer ticker.Stop()
		}
		pending := true
		backoff := 10 * time.Millisecond
		const maxBackoff = time.Second
		var retry <-chan time.Time
		for {
			if pending {
				for {
					select {
					case <-runner.config.Outbox.Notifications():
					default:
						goto drained
					}
				}
			drained:
				err := runner.RunOnce(runCtx)
				runner.mu.Lock()
				runner.lastErr = err
				runner.mu.Unlock()
				pending = false
				if err != nil && runCtx.Err() == nil {
					retry = time.After(backoff)
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				} else {
					retry = nil
					backoff = 10 * time.Millisecond
				}
			}
			select {
			case <-runCtx.Done():
				return
			case <-runner.config.Outbox.Notifications():
				pending = true
			case <-ticks:
				pending = true
			case <-retry:
				retry = nil
				pending = true
			}
		}
	}()
	return nil
}

// LastError returns the most recent background RunOnce failure.
func (runner *DreamingRunner) LastError() error {
	if runner == nil {
		return nil
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.lastErr
}

func (runner *DreamingRunner) Close() error {
	runner.mu.Lock()
	cancel, done := runner.cancel, runner.done
	runner.cancel = nil
	runner.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	return nil
}
