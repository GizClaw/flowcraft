// Package scheduler provides an in-process implementation of the scheduler
// control and leased-work protocols.
package scheduler

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkscheduler "github.com/GizClaw/flowcraft/sdk/scheduler"
	"github.com/robfig/cron/v3"
)

// Timer is the timer capability needed by LocalServer.
type Timer interface {
	Stop() bool
}

// Clock supplies time and one-shot timers. Implementations must be safe for
// concurrent use.
type Clock interface {
	Now() time.Time
	AfterFunc(time.Duration, func()) Timer
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }
func (wallClock) AfterFunc(delay time.Duration, fn func()) Timer {
	return time.AfterFunc(delay, fn)
}

// RuleStore persists recurring rules for Restore.
type RuleStore interface {
	Save(context.Context, sdkscheduler.Rule) error
	Delete(ctx context.Context, namespace, id string) error
	List(context.Context) ([]sdkscheduler.Rule, error)
}

// Option configures a LocalServer.
type Option func(*serverOptions) error

type serverOptions struct {
	store               RuleStore
	clock               Clock
	completionRetention time.Duration
	maxCompletion       time.Duration
	onceRetention       time.Duration
	rollbackTimeout     time.Duration
}

const (
	defaultCompletionRetention = time.Hour
	defaultMaxCompletion       = 24 * time.Hour
	defaultOnceRetention       = time.Hour
	defaultRollbackTimeout     = 5 * time.Second
	scheduleGateStripes        = 64
)

// WithRuleStore enables recurring-rule persistence and startup restoration.
func WithRuleStore(store RuleStore) Option {
	return func(options *serverOptions) error {
		if isNil(store) {
			return errdefs.Validationf("scheduler: rule store must not be nil")
		}
		options.store = store
		return nil
	}
}

// WithClock overrides time and one-shot timers, primarily for deterministic
// tests.
func WithClock(clock Clock) Option {
	return func(options *serverOptions) error {
		if isNil(clock) {
			return errdefs.Validationf("scheduler: clock must not be nil")
		}
		options.clock = clock
		return nil
	}
}

// WithCompletionRetention configures how long terminal Complete requests are
// retained for idempotent replay.
func WithCompletionRetention(retention time.Duration) Option {
	return func(options *serverOptions) error {
		if retention <= 0 {
			return errdefs.Validationf("scheduler: completion retention must be positive")
		}
		options.completionRetention = retention
		return nil
	}
}

// WithMaxCompletionRetention limits caller-requested Complete tombstone
// retention. Requests beyond this bound are rejected as validation errors.
func WithMaxCompletionRetention(retention time.Duration) Option {
	return func(options *serverOptions) error {
		if retention <= 0 {
			return errdefs.Validationf("scheduler: maximum completion retention must be positive")
		}
		options.maxCompletion = retention
		return nil
	}
}

// WithOnceRetention configures how long fired and canceled one-shot requests
// remain available for idempotent replay before automatic cleanup.
func WithOnceRetention(retention time.Duration) Option {
	return func(options *serverOptions) error {
		if retention <= 0 {
			return errdefs.Validationf("scheduler: one-shot retention must be positive")
		}
		options.onceRetention = retention
		return nil
	}
}

// WithRollbackTimeout bounds each RuleStore compensation attempt independently
// from the originating request context.
func WithRollbackTimeout(timeout time.Duration) Option {
	return func(options *serverOptions) error {
		if timeout <= 0 {
			return errdefs.Validationf("scheduler: rollback timeout must be positive")
		}
		options.rollbackTimeout = timeout
		return nil
	}
}

type scheduleKey struct {
	namespace string
	id        string
}

type ruleState struct {
	rule       sdkscheduler.Rule
	entry      cron.EntryID
	generation uint64
}

type onceStatus uint8

const (
	oncePending onceStatus = iota
	onceFired
	onceCanceled
)

type onceState struct {
	once   sdkscheduler.Once
	timer  Timer
	expiry Timer
	status onceStatus
}

type executionState struct {
	execution  sdkscheduler.Execution
	deliveryID string
	leaseToken string
	leaseUntil time.Time
}

type completionTombstone struct {
	request   sdkscheduler.CompleteRequest
	expiresAt time.Time
}

type completionExpiry struct {
	executionID string
	expiresAt   time.Time
}

type completionHeap []completionExpiry

func (h completionHeap) Len() int           { return len(h) }
func (h completionHeap) Less(i, j int) bool { return h[i].expiresAt.Before(h[j].expiresAt) }
func (h completionHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *completionHeap) Push(value any) {
	*h = append(*h, value.(completionExpiry))
}

func (h *completionHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	old[last] = completionExpiry{}
	*h = old[:last]
	return value
}

// LocalServer is a single-process scheduler.Server. Trigger callbacks only
// enqueue protocol executions; business handlers run in sdk/scheduler.Worker.
type LocalServer struct {
	cron  *cron.Cron
	store RuleStore
	clock Clock

	mu          sync.Mutex
	rules       map[scheduleKey]*ruleState
	once        map[scheduleKey]*onceState
	gates       [scheduleGateStripes]sync.Mutex
	executions  map[string]*executionState
	queues      map[string][]string
	completions map[string]completionTombstone
	completeExp completionHeap
	completeTTL time.Duration
	maxComplete time.Duration
	onceTTL     time.Duration
	rollbackTTL time.Duration
	callbacks   sync.WaitGroup
	closeDone   chan struct{}
	started     bool
	restored    bool
	restoring   bool
	restoreDone chan struct{}
	restoreN    int
	restoreErr  error
	closed      bool
}

var _ sdkscheduler.Server = (*LocalServer)(nil)

// NewLocalServer constructs an unstarted local scheduler server.
func NewLocalServer(options ...Option) (*LocalServer, error) {
	config := serverOptions{
		clock:               wallClock{},
		completionRetention: defaultCompletionRetention,
		maxCompletion:       defaultMaxCompletion,
		onceRetention:       defaultOnceRetention,
		rollbackTimeout:     defaultRollbackTimeout,
	}
	for _, option := range options {
		if option == nil {
			return nil, errdefs.Validationf("scheduler: option must not be nil")
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	if config.maxCompletion < config.completionRetention {
		return nil, errdefs.Validationf(
			"scheduler: maximum completion retention must not be shorter than default retention",
		)
	}
	return &LocalServer{
		cron:        cron.New(cron.WithLocation(time.UTC)),
		store:       config.store,
		clock:       config.clock,
		rules:       make(map[scheduleKey]*ruleState),
		once:        make(map[scheduleKey]*onceState),
		executions:  make(map[string]*executionState),
		queues:      make(map[string][]string),
		completions: make(map[string]completionTombstone),
		completeTTL: config.completionRetention,
		maxComplete: config.maxCompletion,
		onceTTL:     config.onceRetention,
		rollbackTTL: config.rollbackTimeout,
		closeDone:   make(chan struct{}),
	}, nil
}

// Start restores persisted rules once, then begins cron evaluation. It is
// idempotent. Valid restored rules remain armed even when Restore reports
// skipped invalid rows.
func (s *LocalServer) Start() error {
	if s == nil {
		return errdefs.Validationf("scheduler: local server is required")
	}
	_, restoreErr := s.Restore(context.Background())
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errdefs.NotAvailablef("scheduler: local server closed")
	}
	if !s.started {
		s.started = true
		s.cron.Start()
	}
	return restoreErr
}

// Close stops cron and one-shot timers, rejects further operations, and waits
// for trigger callbacks admitted before closure. It is idempotent.
func (s *LocalServer) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		return nil
	}
	s.closed = true
	timers := make([]Timer, 0, len(s.once)*2)
	for _, state := range s.once {
		if state.status == oncePending && state.timer != nil {
			timers = append(timers, state.timer)
		}
		if state.expiry != nil {
			timers = append(timers, state.expiry)
		}
	}
	s.mu.Unlock()

	for _, timer := range timers {
		timer.Stop()
	}
	<-s.cron.Stop().Done()
	s.callbacks.Wait()
	close(s.closeDone)
	return nil
}

// Restore arms persisted recurring rules once. Invalid rows are skipped and
// returned as a joined error without preventing valid rows from being restored.
func (s *LocalServer) Restore(ctx context.Context) (int, error) {
	if s == nil {
		return 0, errdefs.Validationf("scheduler: local server is required")
	}
	if ctx == nil {
		return 0, errdefs.Validationf("scheduler: Restore context must not be nil")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, errdefs.NotAvailablef("scheduler: local server closed")
	}
	if s.restored {
		s.mu.Unlock()
		return 0, nil
	}
	if s.restoring {
		done := s.restoreDone
		s.mu.Unlock()
		select {
		case <-done:
			s.mu.Lock()
			count, err := s.restoreN, s.restoreErr
			s.mu.Unlock()
			return count, err
		case <-ctx.Done():
			return 0, errdefs.FromContext(ctx.Err())
		}
	}
	s.restoring = true
	s.restoreDone = make(chan struct{})
	done := s.restoreDone
	s.mu.Unlock()

	count, complete, restoreErr := s.restore(ctx)
	s.mu.Lock()
	s.restoring = false
	s.restored = complete
	s.restoreN = count
	s.restoreErr = restoreErr
	close(done)
	s.mu.Unlock()
	return count, restoreErr
}

func (s *LocalServer) restore(ctx context.Context) (int, bool, error) {
	if s.store == nil {
		return 0, true, nil
	}
	rules, err := s.store.List(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("scheduler: list persisted rules: %w", err)
	}
	count := 0
	var joined []error
	for _, rule := range rules {
		normalized, spec, err := validateRule(rule)
		if err != nil {
			joined = append(joined, fmt.Errorf("scheduler: restore rule %q/%q: %w", rule.Namespace, rule.ID, err))
			continue
		}
		if err := s.putRule(ctx, normalized, spec, false); err != nil {
			joined = append(joined, fmt.Errorf("scheduler: restore rule %q/%q: %w", rule.Namespace, rule.ID, err))
			continue
		}
		count++
	}
	return count, true, errors.Join(joined...)
}

// PutRule creates or replaces a recurring rule by namespace and caller ID.
func (s *LocalServer) PutRule(ctx context.Context, rule sdkscheduler.Rule) error {
	if ctx == nil {
		return errdefs.Validationf("scheduler: PutRule context must not be nil")
	}
	normalized, spec, err := validateRule(rule)
	if err != nil {
		return err
	}
	return s.putRule(ctx, normalized, spec, true)
}

func (s *LocalServer) putRule(
	ctx context.Context,
	rule sdkscheduler.Rule,
	spec string,
	persist bool,
) error {
	key := scheduleKey{namespace: rule.Namespace, id: rule.ID}
	gate, err := s.gate(key)
	if err != nil {
		return err
	}
	gate.Lock()
	defer gate.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errdefs.NotAvailablef("scheduler: local server closed")
	}
	old := s.rules[key]
	if old != nil && rulesEqual(old.rule, rule) {
		s.mu.Unlock()
		return nil
	}
	var oldRule sdkscheduler.Rule
	if old != nil {
		oldRule = old.rule
	}
	s.mu.Unlock()

	if persist && s.store != nil {
		if err := s.store.Save(ctx, rule); err != nil {
			return fmt.Errorf("scheduler: persist rule %q/%q: %w", rule.Namespace, rule.ID, err)
		}
	}

	state := &ruleState{rule: rule}
	if old != nil {
		state.generation = old.generation + 1
	} else {
		state.generation = 1
	}
	generation := state.generation
	entry, err := s.cron.AddFunc(spec, func() { s.fireRule(key, state, generation) })
	if err != nil {
		originalErr := errdefs.Validationf("scheduler: invalid cron %q: %v", rule.Cron, err)
		return errors.Join(originalErr, s.rollbackRule(ctx, rule, oldRule, old != nil, persist))
	}
	state.entry = entry

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.cron.Remove(entry)
		originalErr := errdefs.NotAvailablef("scheduler: local server closed")
		return errors.Join(originalErr, s.rollbackRule(ctx, rule, oldRule, old != nil, persist))
	}
	s.rules[key] = state
	s.mu.Unlock()
	if old != nil {
		s.cron.Remove(old.entry)
	}
	return nil
}

func (s *LocalServer) rollbackRule(
	ctx context.Context,
	rule, old sdkscheduler.Rule,
	hadOld, persist bool,
) error {
	if !persist || s.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.rollbackTTL)
	defer cancel()
	if hadOld {
		if err := s.store.Save(ctx, old); err != nil {
			return fmt.Errorf(
				"scheduler: restore persisted rule %q/%q: %w",
				old.Namespace, old.ID, err,
			)
		}
		return nil
	}
	if err := s.store.Delete(ctx, rule.Namespace, rule.ID); err != nil {
		return fmt.Errorf(
			"scheduler: remove persisted rule %q/%q: %w",
			rule.Namespace, rule.ID, err,
		)
	}
	return nil
}

// DeleteRule removes a recurring rule. Already queued or running executions
// are deliberately left untouched.
func (s *LocalServer) DeleteRule(ctx context.Context, namespace, id string) error {
	if ctx == nil {
		return errdefs.Validationf("scheduler: DeleteRule context must not be nil")
	}
	if err := validateKey(namespace, id); err != nil {
		return err
	}
	key := scheduleKey{namespace: namespace, id: id}
	gate, err := s.gate(key)
	if err != nil {
		return err
	}
	gate.Lock()
	defer gate.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errdefs.NotAvailablef("scheduler: local server closed")
	}
	state := s.rules[key]
	s.mu.Unlock()
	if state == nil {
		return errdefs.NotFoundf("scheduler: rule %q/%q not found", namespace, id)
	}
	if s.store != nil {
		if err := s.store.Delete(ctx, namespace, id); err != nil {
			return fmt.Errorf("scheduler: delete persisted rule %q/%q: %w", namespace, id, err)
		}
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		originalErr := errdefs.NotAvailablef("scheduler: local server closed")
		return errors.Join(originalErr, s.restoreDeletedRule(ctx, state.rule))
	}
	if s.rules[key] != state {
		s.mu.Unlock()
		originalErr := errdefs.Conflictf(
			"scheduler: rule %q/%q changed during delete", namespace, id,
		)
		return errors.Join(originalErr, s.restoreDeletedRule(ctx, state.rule))
	}
	delete(s.rules, key)
	s.mu.Unlock()
	s.cron.Remove(state.entry)
	return nil
}

func (s *LocalServer) restoreDeletedRule(ctx context.Context, rule sdkscheduler.Rule) error {
	if s.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.rollbackTTL)
	defer cancel()
	if err := s.store.Save(ctx, rule); err != nil {
		return fmt.Errorf(
			"scheduler: restore persisted rule %q/%q after delete: %w",
			rule.Namespace, rule.ID, err,
		)
	}
	return nil
}

// ListRules lists recurring rules in one namespace in stable ID order.
func (s *LocalServer) ListRules(
	ctx context.Context,
	namespace string,
) ([]sdkscheduler.Rule, error) {
	if ctx == nil {
		return nil, errdefs.Validationf("scheduler: ListRules context must not be nil")
	}
	if err := validateKeyPart("namespace", namespace); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errdefs.NotAvailablef("scheduler: local server closed")
	}
	rules := make([]sdkscheduler.Rule, 0)
	for key, state := range s.rules {
		if key.namespace == namespace {
			rules = append(rules, cloneRule(state.rule))
		}
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	return rules, nil
}

// ScheduleOnce arms an absolute one-shot schedule. Identical replay is a no-op
// and a different request using the same key is a conflict until the fired or
// canceled record's configured retention expires.
func (s *LocalServer) ScheduleOnce(ctx context.Context, once sdkscheduler.Once) error {
	if ctx == nil {
		return errdefs.Validationf("scheduler: ScheduleOnce context must not be nil")
	}
	if err := once.Validate(); err != nil {
		return err
	}
	once.At = once.At.UTC()
	key := scheduleKey{namespace: once.Namespace, id: once.ID}
	gate, err := s.gate(key)
	if err != nil {
		return err
	}
	gate.Lock()
	defer gate.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errdefs.NotAvailablef("scheduler: local server closed")
	}
	if current := s.once[key]; current != nil {
		if onceEqual(current.once, once) {
			return nil
		}
		return errdefs.Conflictf("scheduler: one-shot %q/%q already exists", once.Namespace, once.ID)
	}
	state := &onceState{once: cloneOnce(once), status: oncePending}
	delay := max(once.At.Sub(s.clock.Now()), 0)
	state.timer = s.clock.AfterFunc(delay, func() { s.fireOnce(key, state) })
	s.once[key] = state
	return nil
}

// CancelOnce cancels a pending one-shot schedule. Canceled records remain
// idempotently cancelable until the configured one-shot retention expires.
func (s *LocalServer) CancelOnce(ctx context.Context, namespace, id string) error {
	if ctx == nil {
		return errdefs.Validationf("scheduler: CancelOnce context must not be nil")
	}
	if err := validateKey(namespace, id); err != nil {
		return err
	}
	key := scheduleKey{namespace: namespace, id: id}
	gate, err := s.gate(key)
	if err != nil {
		return err
	}
	gate.Lock()
	defer gate.Unlock()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errdefs.NotAvailablef("scheduler: local server closed")
	}
	state := s.once[key]
	if state == nil {
		s.mu.Unlock()
		return errdefs.NotFoundf("scheduler: one-shot %q/%q not found", namespace, id)
	}
	switch state.status {
	case onceCanceled:
		s.mu.Unlock()
		return nil
	case onceFired:
		s.mu.Unlock()
		return errdefs.Conflictf("scheduler: one-shot %q/%q already fired", namespace, id)
	default:
		state.status = onceCanceled
		timer := state.timer
		s.armOnceExpiryLocked(key, state)
		s.mu.Unlock()
		timer.Stop()
		return nil
	}
}

func (s *LocalServer) fireOnce(key scheduleKey, state *onceState) {
	s.mu.Lock()
	if s.closed || s.once[key] != state || state.status != oncePending {
		s.mu.Unlock()
		return
	}
	state.status = onceFired
	s.callbacks.Add(1)
	s.mu.Unlock()
	defer s.callbacks.Done()

	gate, err := s.gate(key)
	if err != nil {
		return
	}
	gate.Lock()
	defer gate.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.once[key] != state || state.status != onceFired {
		return
	}
	s.enqueueLocked(key, state.once.Task, state.once.At)
	s.armOnceExpiryLocked(key, state)
}

func (s *LocalServer) armOnceExpiryLocked(key scheduleKey, state *onceState) {
	state.expiry = s.clock.AfterFunc(s.onceTTL, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.once[key] == state && state.status != oncePending {
			delete(s.once, key)
		}
	})
}

func (s *LocalServer) fireRule(key scheduleKey, state *ruleState, generation uint64) {
	s.mu.Lock()
	if s.closed || s.rules[key] != state || state.generation != generation {
		s.mu.Unlock()
		return
	}
	s.callbacks.Add(1)
	s.mu.Unlock()
	defer s.callbacks.Done()

	gate, err := s.gate(key)
	if err != nil {
		return
	}
	gate.Lock()
	defer gate.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.rules[key] != state || state.generation != generation {
		return
	}
	if state.rule.Overlap == sdkscheduler.OverlapSkip && s.hasOutstandingLocked(key) {
		return
	}
	s.enqueueLocked(key, state.rule.Task, s.clock.Now())
}

func (s *LocalServer) enqueueLocked(
	key scheduleKey,
	task sdkscheduler.Task,
	scheduledAt time.Time,
) {
	executionID := "exec-" + newID()
	state := &executionState{
		execution: sdkscheduler.Execution{
			ID:          executionID,
			Namespace:   key.namespace,
			ScheduleID:  key.id,
			Task:        cloneTask(task),
			Status:      sdkscheduler.StatusQueued,
			ScheduledAt: scheduledAt.UTC(),
		},
		deliveryID: "delivery-" + newID(),
	}
	s.executions[executionID] = state
	s.queues[key.namespace] = append(s.queues[key.namespace], executionID)
}

func (s *LocalServer) hasOutstandingLocked(key scheduleKey) bool {
	for _, state := range s.executions {
		if state.execution.Namespace == key.namespace &&
			state.execution.ScheduleID == key.id &&
			state.execution.Status.Outstanding() {
			return true
		}
	}
	return false
}

// Claim leases one queued execution in a namespace. Expired running
// executions are first re-queued with their stable identity.
func (s *LocalServer) Claim(
	ctx context.Context,
	request sdkscheduler.ClaimRequest,
) (*sdkscheduler.Delivery, error) {
	if ctx == nil {
		return nil, errdefs.Validationf("scheduler: Claim context must not be nil")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errdefs.NotAvailablef("scheduler: local server closed")
	}
	now := s.clock.Now().UTC()
	s.pruneCompletionsLocked(now)
	s.requeueExpiredLocked(request.Namespace, now)
	queue := s.queues[request.Namespace]
	for len(queue) > 0 {
		executionID := queue[0]
		queue = queue[1:]
		state := s.executions[executionID]
		if state == nil || state.execution.Status != sdkscheduler.StatusQueued {
			continue
		}
		state.execution.Status = sdkscheduler.StatusRunning
		state.execution.Attempt++
		started := now
		state.execution.StartedAt = &started
		state.leaseToken = "lease-" + newID()
		state.leaseUntil = now.Add(request.LeaseDuration)
		s.queues[request.Namespace] = queue
		return &sdkscheduler.Delivery{
			ID:          state.deliveryID,
			ExecutionID: state.execution.ID,
			Namespace:   state.execution.Namespace,
			ScheduleID:  state.execution.ScheduleID,
			Task:        cloneTask(state.execution.Task),
			Attempt:     state.execution.Attempt,
			LeaseToken:  state.leaseToken,
			LeaseUntil:  state.leaseUntil,
			ScheduledAt: state.execution.ScheduledAt,
		}, nil
	}
	s.queues[request.Namespace] = queue
	return nil, nil
}

func (s *LocalServer) requeueExpiredLocked(namespace string, now time.Time) {
	for _, state := range s.executions {
		if state.execution.Namespace != namespace ||
			state.execution.Status != sdkscheduler.StatusRunning ||
			now.Before(state.leaseUntil) {
			continue
		}
		state.execution.Status = sdkscheduler.StatusQueued
		state.leaseToken = ""
		state.leaseUntil = time.Time{}
		s.queues[namespace] = append(s.queues[namespace], state.execution.ID)
	}
}

// Renew extends a currently owned, unexpired lease.
func (s *LocalServer) Renew(ctx context.Context, request sdkscheduler.RenewRequest) error {
	if ctx == nil {
		return errdefs.Validationf("scheduler: Renew context must not be nil")
	}
	if err := request.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errdefs.NotAvailablef("scheduler: local server closed")
	}
	state := s.executions[request.ExecutionID]
	if state == nil {
		return errdefs.NotFoundf("scheduler: execution %q not found", request.ExecutionID)
	}
	now := s.clock.Now().UTC()
	if state.execution.Status != sdkscheduler.StatusRunning ||
		state.leaseToken != request.LeaseToken ||
		!now.Before(state.leaseUntil) {
		return errdefs.Conflictf("scheduler: execution %q lease is stale", request.ExecutionID)
	}
	state.leaseUntil = now.Add(request.LeaseDuration)
	return nil
}

// Complete settles a currently owned, unexpired lease at a terminal status.
// Exact retries remain idempotent until the later of the default retention and
// request RetainUntil. RetainUntil beyond WithMaxCompletionRetention is
// rejected as validation rather than silently capped.
func (s *LocalServer) Complete(
	ctx context.Context,
	request sdkscheduler.CompleteRequest,
) error {
	if ctx == nil {
		return errdefs.Validationf("scheduler: Complete context must not be nil")
	}
	if err := request.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errdefs.NotAvailablef("scheduler: local server closed")
	}
	state := s.executions[request.ExecutionID]
	now := s.clock.Now().UTC()
	if request.RetainUntil != nil && request.RetainUntil.After(now.Add(s.maxComplete)) {
		return errdefs.Validationf(
			"scheduler: CompleteRequest.RetainUntil exceeds maximum completion retention",
		)
	}
	s.pruneCompletionsLocked(now)
	if state == nil {
		if completed, ok := s.completions[request.ExecutionID]; ok {
			if completeRequestsEqual(completed.request, request) {
				return nil
			}
			return errdefs.Conflictf(
				"scheduler: execution %q was completed by a different request",
				request.ExecutionID,
			)
		}
		return errdefs.NotFoundf("scheduler: execution %q not found", request.ExecutionID)
	}
	if state.execution.Status != sdkscheduler.StatusRunning ||
		state.leaseToken != request.LeaseToken ||
		!now.Before(state.leaseUntil) {
		return errdefs.Conflictf("scheduler: execution %q lease is stale", request.ExecutionID)
	}
	state.execution.Status = request.Status
	state.execution.Error = request.Error
	finished := now
	state.execution.FinishedAt = &finished
	delete(s.executions, request.ExecutionID)
	s.rememberCompletionLocked(request, now)
	return nil
}

func (s *LocalServer) rememberCompletionLocked(
	request sdkscheduler.CompleteRequest,
	completedAt time.Time,
) {
	expiresAt := completedAt.Add(s.completeTTL)
	if request.RetainUntil != nil && request.RetainUntil.After(expiresAt) {
		expiresAt = *request.RetainUntil
	}
	s.completions[request.ExecutionID] = completionTombstone{
		request:   request,
		expiresAt: expiresAt,
	}
	heap.Push(&s.completeExp, completionExpiry{
		executionID: request.ExecutionID,
		expiresAt:   expiresAt,
	})
}

func (s *LocalServer) pruneCompletionsLocked(now time.Time) {
	for s.completeExp.Len() > 0 && !now.Before(s.completeExp[0].expiresAt) {
		expired := heap.Pop(&s.completeExp).(completionExpiry)
		if tombstone, ok := s.completions[expired.executionID]; ok &&
			tombstone.expiresAt.Equal(expired.expiresAt) {
			delete(s.completions, expired.executionID)
		}
	}
	if s.completeExp.Len() == 0 {
		s.completeExp = nil
		if len(s.completions) == 0 {
			s.completions = make(map[string]completionTombstone)
		}
	}
}

func (s *LocalServer) gate(key scheduleKey) (*sync.Mutex, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errdefs.NotAvailablef("scheduler: local server closed")
	}
	return s.gateForKey(key), nil
}

func (s *LocalServer) gateForKey(key scheduleKey) *sync.Mutex {
	const (
		offset64 = uint64(14695981039346656037)
		prime64  = uint64(1099511628211)
	)
	hash := offset64
	for i := 0; i < len(key.namespace); i++ {
		hash ^= uint64(key.namespace[i])
		hash *= prime64
	}
	hash ^= 0
	hash *= prime64
	for i := 0; i < len(key.id); i++ {
		hash ^= uint64(key.id[i])
		hash *= prime64
	}
	return &s.gates[hash%scheduleGateStripes]
}

func validateRule(rule sdkscheduler.Rule) (sdkscheduler.Rule, string, error) {
	if rule.Timezone == "" {
		rule.Timezone = "UTC"
	}
	if err := rule.Validate(); err != nil {
		return sdkscheduler.Rule{}, "", err
	}
	spec := "CRON_TZ=" + rule.Timezone + " " + rule.Cron
	parser := cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
	)
	if _, err := parser.Parse(spec); err != nil {
		return sdkscheduler.Rule{}, "", errdefs.Validationf(
			"scheduler: invalid cron %q: %v", rule.Cron, err,
		)
	}
	return cloneRule(rule), spec, nil
}

func validateKey(namespace, id string) error {
	if err := validateKeyPart("namespace", namespace); err != nil {
		return err
	}
	return validateKeyPart("schedule ID", id)
}

func validateKeyPart(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return errdefs.Validationf("scheduler: %s is required", field)
	}
	if strings.ContainsRune(value, '\x00') {
		return errdefs.Validationf("scheduler: %s must not contain NUL", field)
	}
	return nil
}

func rulesEqual(left, right sdkscheduler.Rule) bool {
	return reflect.DeepEqual(left, right)
}

func onceEqual(left, right sdkscheduler.Once) bool {
	return left.Namespace == right.Namespace &&
		left.ID == right.ID &&
		left.At.Equal(right.At) &&
		reflect.DeepEqual(left.Task, right.Task)
}

func completeRequestsEqual(left, right sdkscheduler.CompleteRequest) bool {
	if left.ExecutionID != right.ExecutionID ||
		left.LeaseToken != right.LeaseToken ||
		left.Status != right.Status ||
		left.Error != right.Error {
		return false
	}
	if left.RetainUntil == nil || right.RetainUntil == nil {
		return left.RetainUntil == nil && right.RetainUntil == nil
	}
	return left.RetainUntil.Equal(*right.RetainUntil)
}

func cloneRule(rule sdkscheduler.Rule) sdkscheduler.Rule {
	rule.Task = cloneTask(rule.Task)
	return rule
}

func cloneOnce(once sdkscheduler.Once) sdkscheduler.Once {
	once.Task = cloneTask(once.Task)
	return once
}

func cloneTask(task sdkscheduler.Task) sdkscheduler.Task {
	task.Payload.Data = append([]byte(nil), task.Payload.Data...)
	return task
}

func isNil(value interface{}) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
